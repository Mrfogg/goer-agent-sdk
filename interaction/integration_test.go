package interaction

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	base "github.com/Mrfogg/goer-agent-sdk"
	"github.com/Mrfogg/goer-agent-sdk/ctxkey"
)

// scriptedLLM is a minimal OpenAI-compatible streaming endpoint: it serves the
// given SSE bodies in order, one per request. It is local to this test so the
// package stays independent of the base package's own test scaffolding (which an
// in-package test cannot import).
type scriptedLLM struct {
	mu      sync.Mutex
	replies []string
	served  int
}

func (s *scriptedLLM) start(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		reply := ""
		if s.served < len(s.replies) {
			reply = s.replies[s.served]
		}
		s.served++
		s.mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, reply)
	}))
	t.Cleanup(server.Close)
	return server
}

func (s *scriptedLLM) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.served
}

// sseToolCall scripts one streaming assistant reply that calls a single tool.
func sseToolCall(t *testing.T, id, name, args string) string {
	t.Helper()

	frame := func(payload map[string]any) string {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal frame: %v", err)
		}
		return "data: " + string(encoded) + "\n\n"
	}

	var b strings.Builder
	b.WriteString(frame(map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"index": 0,
					"id":    id,
					"type":  "function",
					"function": map[string]any{
						"name":      name,
						"arguments": args,
					},
				}},
			},
		}},
	}))
	b.WriteString(frame(map[string]any{
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "tool_calls",
		}},
	}))
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

// finishTool is the end tool the run must reach to finish.
type finishTool struct{}

func (finishTool) Name() string        { return "finish" }
func (finishTool) Description() string { return "Submit the final answer" }

func (finishTool) Execute(_ context.Context, args map[string]any) (base.ToolResult, error) {
	answer, _ := args["answer"].(string)
	return base.ToolResult{Success: true, ModelContent: strings.TrimSpace(answer)}, nil
}

func TestRunSuspendsOnPermissionAndResumesWithTheAnswer(t *testing.T) {
	llm := &scriptedLLM{replies: []string{
		sseToolCall(t, "call_1", RequestPermissionToolName, `{"action":"delete every row","reason":"cleanup"}`),
		sseToolCall(t, "call_2", "finish", `{"answer":"done"}`),
	}}
	server := llm.start(t)

	registry := NewRegistry("s1-")
	agent := base.NewBaseAgent(
		"test", "test agent", "You are a test agent.",
		"test-model", "test-token", server.URL,
		finishTool{},
	)
	agent.AddTool(RequestPermissionTool{AllowAlways: true})

	ctx := context.WithValue(context.Background(), ctxkey.InteractionRegistry, registry)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	stream, err := agent.Run(ctx, "clean up the sheet")
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	var (
		sawRequest  bool
		requestKind string
		requestID   string
	)

	deadline := time.After(15 * time.Second)
drain:
	for {
		select {
		case msg, open := <-stream:
			if !open {
				break drain
			}
			if msg.Type != MsgTypeRequest {
				continue
			}
			sawRequest = true
			requestID, _ = msg.Data["id"].(string)
			requestKind, _ = msg.Data["kind"].(string)
			// Answer from "the network side", just as a transport would once the
			// client clicks Allow.
			if err := registry.Resolve(Response{ID: requestID, Choice: permissionAllow}); err != nil {
				t.Fatalf("resolve: %v", err)
			}
		case <-deadline:
			t.Fatal("the run did not finish")
		}
	}

	if !sawRequest {
		t.Fatal("the run never asked for permission")
	}
	if requestKind != string(KindPermission) {
		t.Fatalf("request kind = %q, want %q", requestKind, KindPermission)
	}

	result := agent.Result()
	if result.Err != nil || result.Stopped {
		t.Fatalf("run outcome = %+v", result)
	}
	if result.Answer != "done" {
		t.Fatalf("answer = %q, want %q", result.Answer, "done")
	}
	if llm.count() != 2 {
		t.Fatalf("llm calls = %d, want 2 (permission + finish)", llm.count())
	}

	// The user's answer must be recorded as the tool result the model sees.
	var content string
	var found bool
	for _, message := range agent.History() {
		if message.ToolCallID == "call_1" {
			content = message.Content
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("no tool result recorded for call_1: %s", describeHistory(t, agent))
	}
	if !strings.Contains(content, `"approved":true`) {
		t.Fatalf("tool result = %s, want it to carry approved:true", content)
	}
}

func describeHistory(t *testing.T, agent *base.BaseAgent) string {
	t.Helper()
	var parts []string
	for _, message := range agent.History() {
		parts = append(parts, message.Role+":"+message.ToolCallID)
	}
	return strings.Join(parts, ",")
}
