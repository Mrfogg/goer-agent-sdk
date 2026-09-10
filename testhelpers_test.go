package base

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sashabaranov/go-openai"
)

// fakeLLMBlock makes the fake endpoint hold the request open until the client
// disconnects, which lets tests exercise cancellation.
const fakeLLMBlock = "__block__"

type fakeLLMRequest struct {
	authorization string
	toolChoice    any
	messages      []openai.ChatCompletionMessage
	headers       http.Header
}

// fakeLLM is an OpenAI-compatible streaming endpoint driven by a script of SSE
// response bodies. It records every request it receives.
type fakeLLM struct {
	mu       sync.Mutex
	script   []string
	requests []fakeLLMRequest
}

func newFakeLLM(script ...string) *fakeLLM {
	return &fakeLLM{script: script}
}

func (f *fakeLLM) start(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(server.Close)
	return server
}

func (f *fakeLLM) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var request struct {
		ToolChoice any                            `json:"tool_choice"`
		Messages   []openai.ChatCompletionMessage `json:"messages"`
	}
	_ = json.Unmarshal(body, &request)

	f.mu.Lock()
	f.requests = append(f.requests, fakeLLMRequest{
		authorization: r.Header.Get("Authorization"),
		toolChoice:    request.ToolChoice,
		messages:      request.Messages,
		headers:       r.Header.Clone(),
	})
	next := ""
	if len(f.script) > 0 {
		next, f.script = f.script[0], f.script[1:]
	}
	f.mu.Unlock()

	if next == fakeLLMBlock {
		<-r.Context().Done()
		return
	}
	if next == "" {
		http.Error(w, "fake llm: script exhausted", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, next)
}

func (f *fakeLLM) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeLLM) requestAt(t *testing.T, index int) fakeLLMRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if index >= len(f.requests) {
		t.Fatalf("request %d was not recorded (have %d)", index, len(f.requests))
	}
	return f.requests[index]
}

type scriptedToolCall struct {
	id        string
	name      string
	arguments string
}

// sseText scripts a text-only assistant reply.
func sseText(t *testing.T, content string) string {
	t.Helper()
	chunk := map[string]any{
		"choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": content}},
		},
	}
	return "data: " + mustJSON(t, chunk) + "\n\ndata: [DONE]\n\n"
}

// sseReasoning scripts an assistant reply that streams reasoning (thinking) text.
func sseReasoning(t *testing.T, reasoning string) string {
	t.Helper()
	chunk := map[string]any{
		"choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "reasoning_content": reasoning}},
		},
	}
	return "data: " + mustJSON(t, chunk) + "\n\ndata: [DONE]\n\n"
}

// sseToolCalls scripts an assistant reply carrying tool calls.
func sseToolCalls(t *testing.T, calls ...scriptedToolCall) string {
	t.Helper()
	deltaCalls := make([]any, 0, len(calls))
	for i, call := range calls {
		deltaCalls = append(deltaCalls, map[string]any{
			"index": i,
			"id":    call.id,
			"type":  "function",
			"function": map[string]any{
				"name":      call.name,
				"arguments": call.arguments,
			},
		})
	}

	var b strings.Builder
	b.WriteString("data: " + mustJSON(t, map[string]any{
		"choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": deltaCalls}},
		},
	}) + "\n\n")
	b.WriteString("data: " + mustJSON(t, map[string]any{
		"choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"},
		},
	}) + "\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(encoded)
}

// stubTool is a configurable Tool for tests.
type stubTool struct {
	name    string
	content string
	result  ToolResult
	execute func(ctx context.Context, args map[string]any) (ToolResult, error)
}

func newStubTool(name, content string) *stubTool {
	return &stubTool{
		name:    name,
		content: content,
		result:  ToolResult{Success: true, ModelContent: content},
	}
}

func (s *stubTool) Name() string        { return s.name }
func (s *stubTool) Description() string { return "stub tool " + s.name }

func (s *stubTool) Execute(ctx context.Context, args map[string]any) (ToolResult, error) {
	if s.execute != nil {
		return s.execute(ctx, args)
	}
	result := s.result
	if result.ModelContent == "" {
		result.ModelContent = s.content
	}
	return result, nil
}

// startAgent builds an agent wired to the fake endpoint. Without explicit tools
// it registers a single end tool named "finish".
func startAgent(t *testing.T, server *httptest.Server, tools ...Tool) *BaseAgent {
	t.Helper()
	if len(tools) == 0 {
		tools = []Tool{newStubTool("finish", "final answer")}
	}
	return NewBaseAgent("test-agent", "test agent", "You are a test agent.", "test-model", "test-token", server.URL, tools...)
}

func collectRun(t *testing.T, agent *BaseAgent, input string) []Msg {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return drain(t, agent.Run(ctx, input))
}

func drain(t *testing.T, msgChan chan Msg) []Msg {
	t.Helper()
	var msgs []Msg
	timeout := time.After(30 * time.Second)
	for {
		select {
		case msg, open := <-msgChan:
			if !open {
				return msgs
			}
			msgs = append(msgs, msg)
		case <-timeout:
			t.Fatalf("run channel did not close; received %v", msgTypes(msgs))
			return msgs
		}
	}
}

func waitForEvent(t *testing.T, msgChan chan Msg) Msg {
	t.Helper()
	select {
	case msg, open := <-msgChan:
		if !open {
			t.Fatal("run channel closed before the first event")
		}
		return msg
	case <-time.After(10 * time.Second):
		t.Fatal("no run event received")
		return Msg{}
	}
}

func lastMessageOfType(msgs []Msg, msgType string) (Msg, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Type == msgType {
			return msgs[i], true
		}
	}
	return Msg{}, false
}

func msgTypes(msgs []Msg) []string {
	types := make([]string, 0, len(msgs))
	for _, msg := range msgs {
		types = append(types, msg.Type)
	}
	return types
}

func firstSystemMessage(messages []openai.ChatCompletionMessage) string {
	for _, message := range messages {
		if message.Role == openai.ChatMessageRoleSystem {
			return message.Content
		}
	}
	return ""
}

func containsUserMessage(messages []openai.ChatCompletionMessage, fragment string) bool {
	for _, message := range messages {
		if message.Role == openai.ChatMessageRoleUser && strings.Contains(message.Content, fragment) {
			return true
		}
	}
	return false
}

func toolHistoryMessages(history []openai.ChatCompletionMessage) []openai.ChatCompletionMessage {
	var tools []openai.ChatCompletionMessage
	for _, message := range history {
		if message.Role == openai.ChatMessageRoleTool {
			tools = append(tools, message)
		}
	}
	return tools
}

func toolMessageByID(history []openai.ChatCompletionMessage, toolCallID string) (openai.ChatCompletionMessage, bool) {
	for _, message := range toolHistoryMessages(history) {
		if message.ToolCallID == toolCallID {
			return message, true
		}
	}
	return openai.ChatCompletionMessage{}, false
}

func describeHistory(history []openai.ChatCompletionMessage) string {
	parts := make([]string, 0, len(history))
	for _, message := range history {
		parts = append(parts, fmt.Sprintf("%s:%d", message.Role, len(message.Content)))
	}
	return strings.Join(parts, ",")
}
