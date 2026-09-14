package base

import (
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"
)

// TestRunCompactsContextWhenPromptTokensCrossThreshold drives one run across three
// model calls and checks the compaction wiring end to end: the trigger is the
// prompt_tokens the previous call reported, the summarizer is a plain non-streaming
// call with no tools, and the request after compaction carries the block instead of
// the compacted span.
func TestRunCompactsContextWhenPromptTokensCrossThreshold(t *testing.T) {
	bigResult := strings.Repeat("x", 40000)
	llm := newFakeLLM(
		sseToolCallsWithUsage(t, 10, 5, scriptedToolCall{id: "call_1", name: "huge", arguments: "{}"}),
		sseToolCallsWithUsage(t, 5000, 5, scriptedToolCall{id: "call_2", name: "huge", arguments: "{}"}),
		sseToolCallsWithUsage(t, 10, 5, scriptedToolCall{id: "call_3", name: "finish", arguments: `{"answer":"done"}`}),
	)
	const summary = "### 1. Main intent\nkeep going"
	// The summarizer is called non-streaming, so the fake serves it from the JSON script.
	llm.jsonScript = []string{jsonReply(t, "", summary)}
	server := llm.start(t)

	agent := NewBaseAgent("test-agent", "test agent", "You are a test agent.", "test-model", "test-token", server.URL,
		newStubTool("finish", "done")).
		WithMaxContextTokens(1000) // triggers past 200 prompt tokens, keeps 50 verbatim
	agent.AddTool(newStubTool("huge", bigResult))

	_, result := collectRun(t, agent, "do the thing")
	if result.Err != nil {
		t.Fatalf("run failed: %v", result.Err)
	}
	if result.Answer != "done" {
		t.Fatalf("answer = %q, want %q", result.Answer, "done")
	}
	if got := llm.requestCount(); got != 4 {
		t.Fatalf("LLM requests = %d, want 4 (3 agent calls + 1 summarizer)", got)
	}

	// The summarizer call is the only non-streaming request: fixed instruction, the
	// rendered span, and no tools.
	summarizer := requestAtMatching(t, llm, func(request fakeLLMRequest) bool { return !request.stream })
	if len(summarizer.tools) != 0 {
		t.Fatalf("summarizer request carried %d tools, want none", len(summarizer.tools))
	}
	if !strings.Contains(firstSystemMessage(summarizer.messages), "### 5. All user messages") {
		t.Fatalf("summarizer request is missing the fixed instruction, got: %.120q", firstSystemMessage(summarizer.messages))
	}
	if !containsUserMessage(summarizer.messages, "do the thing") {
		t.Fatalf("summarizer request is missing the rendered span")
	}

	// The request after compaction carries the block where the compacted span used to be.
	last := llm.requestAt(t, llm.requestCount()-1)
	if !last.stream {
		t.Fatalf("the last request should be the streamed agent call")
	}
	if !containsUserMessage(last.messages, "<compacted-history>") {
		t.Fatalf("post-compaction request is missing the compaction block: %s", describeHistory(last.messages))
	}
	if !containsUserMessage(last.messages, summary) {
		t.Fatalf("compaction block is missing the summary text")
	}
	if !containsUserMessage(last.messages, "do the thing") {
		t.Fatalf("compaction block is missing the verbatim user message section")
	}
	if !messageContainsToolResult(last.messages, "call_2", bigResult) {
		t.Fatalf("the verbatim tail (call_2 result) was not preserved")
	}
	if messageContainsToolResult(last.messages, "call_1", bigResult) {
		t.Fatalf("the compacted span (call_1 result) is still in the request")
	}
	if len(last.tools) == 0 {
		t.Fatalf("the agent request lost its tool definitions")
	}

	// The agent's own history mirrors the compacted state.
	history := agent.History()
	if len(history) != 5 || !isCompactionBlock(history[0]) {
		t.Fatalf("history = %s, want a compaction block followed by 4 messages", describeHistory(history))
	}
	if _, ok := toolMessageByID(history, "call_1"); ok {
		t.Fatalf("the compacted tool result call_1 is still in the history: %s", describeHistory(history))
	}
}

// TestRunKeepsHistoryWhenSummarizerFails checks the failure contract: a failing
// summarizer aborts the compaction and leaves the transcript untouched.
func TestRunKeepsHistoryWhenSummarizerFails(t *testing.T) {
	bigResult := strings.Repeat("x", 40000)
	llm := newFakeLLM(
		sseToolCallsWithUsage(t, 10, 5, scriptedToolCall{id: "call_1", name: "huge", arguments: "{}"}),
		sseToolCallsWithUsage(t, 5000, 5, scriptedToolCall{id: "call_2", name: "huge", arguments: "{}"}),
		sseToolCallsWithUsage(t, 10, 5, scriptedToolCall{id: "call_3", name: "finish", arguments: `{"answer":"done"}`}),
	)
	// No JSON reply is scripted, so the summarizer call fails with a script-exhausted 500.
	server := llm.start(t)

	agent := NewBaseAgent("test-agent", "test agent", "You are a test agent.", "test-model", "test-token", server.URL,
		newStubTool("finish", "done")).
		WithMaxContextTokens(1000)
	agent.AddTool(newStubTool("huge", bigResult))

	_, result := collectRun(t, agent, "do the thing")
	if result.Err != nil {
		t.Fatalf("a failed compaction must not fail the run: %v", result.Err)
	}

	// The transcript still holds the full span, and the next request goes out whole.
	history := agent.History()
	if _, ok := toolMessageByID(history, "call_1"); !ok {
		t.Fatalf("history lost the compacted span after a failed compaction: %s", describeHistory(history))
	}
	last := llm.requestAt(t, llm.requestCount()-1)
	if containsUserMessage(last.messages, "<compacted-history>") {
		t.Fatalf("a failed compaction must not inject a block")
	}
	if !messageContainsToolResult(last.messages, "call_1", bigResult) {
		t.Fatalf("the untouched span should still be in the request")
	}
}

func requestAtMatching(t *testing.T, llm *fakeLLM, match func(fakeLLMRequest) bool) fakeLLMRequest {
	t.Helper()
	llm.mu.Lock()
	defer llm.mu.Unlock()
	for _, request := range llm.requests {
		if match(request) {
			return request
		}
	}
	t.Fatalf("no recorded request matched (have %d)", len(llm.requests))
	return fakeLLMRequest{}
}

func messageContainsToolResult(messages []openai.ChatCompletionMessage, toolCallID, fragment string) bool {
	for _, message := range messages {
		if message.Role == openai.ChatMessageRoleTool && message.ToolCallID == toolCallID && strings.Contains(message.Content, fragment) {
			return true
		}
	}
	return false
}
