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
	tools         []openai.Tool
	messages      []openai.ChatCompletionMessage
	headers       http.Header
	stream        bool
	includeUsage  bool
}

// Token counts the scripted replies report, so tests can assert what the agent
// forwards without depending on a real model.
const (
	testPromptTokens     = 10
	testCompletionTokens = 5
)

// fakeLLM is an OpenAI-compatible endpoint driven by two scripts: SSE bodies for
// streaming requests and JSON bodies for non-streaming ones. It records every
// request it receives.
type fakeLLM struct {
	mu         sync.Mutex
	script     []string
	jsonScript []string
	requests   []fakeLLMRequest
}

func newFakeLLM(script ...string) *fakeLLM {
	return &fakeLLM{script: script}
}

// newFakeLLMWithJSON serves non-streaming replies (plain JSON, no SSE).
func newFakeLLMWithJSON(replies ...string) *fakeLLM {
	return &fakeLLM{jsonScript: replies}
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
		ToolChoice    any                            `json:"tool_choice"`
		Tools         []openai.Tool                  `json:"tools"`
		Messages      []openai.ChatCompletionMessage `json:"messages"`
		Stream        bool                           `json:"stream"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	_ = json.Unmarshal(body, &request)

	includeUsage := request.StreamOptions != nil && request.StreamOptions.IncludeUsage
	f.mu.Lock()
	f.requests = append(f.requests, fakeLLMRequest{
		authorization: r.Header.Get("Authorization"),
		toolChoice:    request.ToolChoice,
		tools:         request.Tools,
		messages:      request.Messages,
		headers:       r.Header.Clone(),
		stream:        request.Stream,
		includeUsage:  includeUsage,
	})
	next := ""
	if request.Stream {
		if len(f.script) > 0 {
			next, f.script = f.script[0], f.script[1:]
		}
	} else if len(f.jsonScript) > 0 {
		next, f.jsonScript = f.jsonScript[0], f.jsonScript[1:]
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

	if !request.Stream {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, next)
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
	return sseTextWithUsage(t, content, testPromptTokens, testCompletionTokens)
}

// sseTextWithUsage scripts a text-only reply with explicit token counts.
func sseTextWithUsage(t *testing.T, content string, promptTokens, completionTokens int) string {
	t.Helper()
	chunk := map[string]any{
		"choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": content}},
		},
	}
	return "data: " + mustJSON(t, chunk) + "\n\n" + sseUsage(t, promptTokens, completionTokens)
}

// sseReasoning scripts an assistant reply that streams reasoning (thinking) text.
func sseReasoning(t *testing.T, reasoning string) string {
	t.Helper()
	chunk := map[string]any{
		"choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "reasoning_content": reasoning}},
		},
	}
	return "data: " + mustJSON(t, chunk) + "\n\n" + sseUsage(t, testPromptTokens, testCompletionTokens)
}

// sseUsage scripts the final stream chunk that carries token usage, which is what
// providers send when the request asks for stream_options.include_usage.
func sseUsage(t *testing.T, promptTokens, completionTokens int) string {
	t.Helper()
	chunk := map[string]any{
		"choices": []any{},
		"model":   "test-model",
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
			"completion_tokens_details": map[string]any{
				"reasoning_tokens": 1,
			},
		},
	}
	return "data: " + mustJSON(t, chunk) + "\n\ndata: [DONE]\n\n"
}

// jsonReply scripts a non-streaming chat completion reply.
func jsonReply(t *testing.T, reasoning, content string, calls ...scriptedToolCall) string {
	t.Helper()

	message := map[string]any{"role": "assistant"}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}
	if content != "" {
		message["content"] = content
	}
	finishReason := "stop"
	if len(calls) > 0 {
		finishReason = "tool_calls"
		toolCalls := make([]any, 0, len(calls))
		for _, call := range calls {
			toolCalls = append(toolCalls, map[string]any{
				"id":   call.id,
				"type": "function",
				"function": map[string]any{
					"name":      call.name,
					"arguments": call.arguments,
				},
			})
		}
		message["tool_calls"] = toolCalls
	}

	return mustJSON(t, map[string]any{
		"id":      "chatcmpl-test",
		"object":  "chat.completion",
		"created": 0,
		"model":   "test-model",
		"usage": map[string]any{
			"prompt_tokens":     testPromptTokens,
			"completion_tokens": testCompletionTokens,
			"total_tokens":      testPromptTokens + testCompletionTokens,
		},
		"choices": []any{
			map[string]any{"index": 0, "message": message, "finish_reason": finishReason},
		},
	})
}

// sseToolCalls scripts an assistant reply carrying tool calls.
func sseToolCalls(t *testing.T, calls ...scriptedToolCall) string {
	t.Helper()
	return sseToolCallsWithUsage(t, testPromptTokens, testCompletionTokens, calls...)
}

// sseToolCallsWithUsage scripts a tool-calling reply with explicit token counts.
func sseToolCallsWithUsage(t *testing.T, promptTokens, completionTokens int, calls ...scriptedToolCall) string {
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
	b.WriteString(sseUsage(t, promptTokens, completionTokens))
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

// collectRun runs one turn and returns the streamed messages together with the
// run outcome.
func collectRun(t *testing.T, agent *BaseAgent, input string) ([]Msg, RunResult) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stream := startStream(t, agent, ctx, input)
	return drain(t, stream), agent.Result()
}

func startStream(t *testing.T, agent *BaseAgent, ctx context.Context, input string) chan Msg {
	t.Helper()
	stream, err := agent.Run(ctx, input)
	if err != nil {
		t.Fatalf("run %q: %v", input, err)
	}
	return stream
}

// waitForRequest waits until the fake LLM has received at least want requests.
// Runs that hit a blocking scripted response emit no messages, so tests cannot
// wait for the first message before cancelling.
func waitForRequest(t *testing.T, llm *fakeLLM, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if llm.requestCount() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the fake LLM received %d requests, want at least %d", llm.requestCount(), want)
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

func lastMessageOfType(msgs []Msg, msgType string) (Msg, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Type == msgType {
			return msgs[i], true
		}
	}
	return Msg{}, false
}

func messagesOfType(msgs []Msg, msgType string) []Msg {
	var matched []Msg
	for _, msg := range msgs {
		if msg.Type == msgType {
			matched = append(matched, msg)
		}
	}
	return matched
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
