package base

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRunFinishesThroughEndTool(t *testing.T) {
	llm := newFakeLLM(sseToolCalls(t, scriptedToolCall{id: "call_1", name: "finish", arguments: `{"answer":"done"}`}))
	server := llm.start(t)
	agent := startAgent(t, server)

	msgs, result := collectRun(t, agent, "make a report")

	if result.Err != nil || result.Stopped {
		t.Fatalf("run outcome = %+v (messages: %v)", result, msgTypes(msgs))
	}
	if result.Answer != "final answer" {
		t.Fatalf("answer = %q, want %q", result.Answer, "final answer")
	}
	if llm.requestCount() != 1 {
		t.Fatalf("llm calls = %d, want 1", llm.requestCount())
	}
	first := llm.requestAt(t, 0)
	if first.authorization != "Bearer test-token" {
		t.Fatalf("authorization = %q", first.authorization)
	}
	if system := firstSystemMessage(first.messages); !strings.Contains(system, "END-TOOL MODE") {
		t.Fatalf("system prompt missing end-tool mode:\n%s", system)
	}
	if _, ok := toolMessageByID(agent.History(), "call_1"); !ok {
		t.Fatalf("missing tool result for call_1: %s", describeHistory(agent.History()))
	}
}

func TestOnlyReasoningAndContentAreEmitted(t *testing.T) {
	llm := newFakeLLM(
		sseReasoning(t, "let me think about it"),
		sseText(t, "the answer is 42"),
		sseToolCalls(t, scriptedToolCall{id: "call_1", name: "finish", arguments: "{}"}),
	)
	server := llm.start(t)
	agent := startAgent(t, server)

	msgs, result := collectRun(t, agent, "hi")

	if result.Err != nil {
		t.Fatalf("run failed: %v", result.Err)
	}
	for _, msg := range msgs {
		if msg.Type != MsgTypeReasoning && msg.Type != MsgTypeContent {
			t.Fatalf("unexpected message type %q in %v", msg.Type, msgTypes(msgs))
		}
	}

	reasoning, ok := lastMessageOfType(msgs, MsgTypeReasoning)
	if !ok || reasoning.Content != "let me think about it" {
		t.Fatalf("reasoning message = %+v (messages: %v)", reasoning, msgTypes(msgs))
	}
	content, ok := lastMessageOfType(msgs, MsgTypeContent)
	if !ok || content.Content != "the answer is 42" {
		t.Fatalf("content message = %+v (messages: %v)", content, msgTypes(msgs))
	}
	for _, msg := range msgs {
		if msg.Type == MsgTypeContent && strings.Contains(msg.Content, "think") {
			t.Fatalf("reasoning text leaked into the content stream: %q", msg.Content)
		}
		if msg.Type == MsgTypeReasoning && strings.Contains(msg.Content, "42") {
			t.Fatalf("answer text leaked into the reasoning stream: %q", msg.Content)
		}
	}
}

func TestTextOnlyReplyIsRejectedAndRetried(t *testing.T) {
	llm := newFakeLLM(
		sseText(t, "I would rather just explain the answer."),
		sseToolCalls(t, scriptedToolCall{id: "call_1", name: "finish", arguments: "{}"}),
	)
	server := llm.start(t)
	agent := startAgent(t, server)

	_, result := collectRun(t, agent, "hi")

	if result.Err != nil || result.Answer != "final answer" {
		t.Fatalf("run outcome = %+v", result)
	}
	if llm.requestCount() != 2 {
		t.Fatalf("llm calls = %d, want 2", llm.requestCount())
	}
	second := llm.requestAt(t, 1)
	if second.toolChoice != "auto" {
		t.Fatalf("tool_choice = %v, want auto below the escalation threshold", second.toolChoice)
	}
	if !containsUserMessage(second.messages, "did not call any tool") {
		t.Fatalf("corrective user message missing:\n%s", describeHistory(second.messages))
	}
}

func TestRepeatedTextOnlyRepliesEscalateAndFail(t *testing.T) {
	llm := newFakeLLM(
		sseText(t, "one"), sseText(t, "two"), sseText(t, "three"),
		sseText(t, "four"), sseText(t, "five"), sseText(t, "six"),
	)
	server := llm.start(t)
	agent := startAgent(t, server)

	_, result := collectRun(t, agent, "hi")

	if result.Err == nil || !strings.Contains(result.Err.Error(), "without calling a tool") {
		t.Fatalf("run outcome = %+v", result)
	}
	if result.Stopped {
		t.Fatalf("a stuck model is a failure, not a cancellation: %+v", result)
	}
	if got := llm.requestAt(t, 2).toolChoice; got != "required" {
		t.Fatalf("tool_choice on the 3rd call = %v, want required", got)
	}
	if llm.requestCount() != noToolCallFailAfter {
		t.Fatalf("llm calls = %d, want %d", llm.requestCount(), noToolCallFailAfter)
	}
}

func TestFailedEndToolDoesNotFinishRun(t *testing.T) {
	var attempts int32
	finish := &stubTool{
		name: "finish",
		execute: func(ctx context.Context, args map[string]any) (ToolResult, error) {
			if atomic.AddInt32(&attempts, 1) == 1 {
				return ToolResult{Success: false, Error: "not ready"}, nil
			}
			return ToolResult{Success: true, ModelContent: "final answer"}, nil
		},
	}
	llm := newFakeLLM(
		sseToolCalls(t, scriptedToolCall{id: "call_1", name: "finish", arguments: "{}"}),
		sseToolCalls(t, scriptedToolCall{id: "call_2", name: "finish", arguments: "{}"}),
	)
	server := llm.start(t)
	agent := startAgent(t, server, finish)

	_, result := collectRun(t, agent, "hi")

	if result.Err != nil || result.Answer != "final answer" {
		t.Fatalf("expected the run to continue after a failed end tool, got %+v", result)
	}
	if llm.requestCount() != 2 {
		t.Fatalf("llm calls = %d, want 2", llm.requestCount())
	}
	failed, ok := toolMessageByID(agent.History(), "call_1")
	if !ok || !strings.Contains(failed.Content, "not ready") {
		t.Fatalf("failed end tool result missing: %+v", failed)
	}
}

func TestSkippedToolCallsAreRecorded(t *testing.T) {
	llm := newFakeLLM(sseToolCalls(t,
		scriptedToolCall{id: "call_end", name: "finish", arguments: "{}"},
		scriptedToolCall{id: "call_late", name: "regular", arguments: "{}"},
	))
	server := llm.start(t)
	agent := startAgent(t, server, newStubTool("finish", "final answer"), newStubTool("regular", "regular result"))

	_, result := collectRun(t, agent, "hi")

	if result.Err != nil {
		t.Fatalf("run failed: %v", result.Err)
	}
	history := agent.History()
	endResult, ok := toolMessageByID(history, "call_end")
	if !ok || !strings.Contains(endResult.Content, `"success":true`) {
		t.Fatalf("end tool result = %+v", endResult)
	}
	lateResult, ok := toolMessageByID(history, "call_late")
	if !ok {
		t.Fatalf("skipped call must still have a tool result: %s", describeHistory(history))
	}
	if !strings.Contains(lateResult.Content, "skipped") {
		t.Fatalf("skipped tool result = %q", lateResult.Content)
	}
}

func TestStopMarksRunStopped(t *testing.T) {
	llm := newFakeLLM(fakeLLMBlock)
	server := llm.start(t)
	agent := startAgent(t, server)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // unblocks the fake LLM even when the test fails early

	stream := startStream(t, agent, ctx, "hi")
	waitForRequest(t, llm, 1)
	agent.Stop()
	drain(t, stream)

	result := agent.Result()
	if !result.Stopped {
		t.Fatalf("run outcome = %+v, want Stopped", result)
	}
	if !errors.Is(result.Err, context.Canceled) {
		t.Fatalf("stop reason = %v, want context.Canceled", result.Err)
	}
	if result.Answer != "" {
		t.Fatalf("a stopped run must not produce an answer, got %q", result.Answer)
	}
}

func TestToolPanicIsReportedAsFailure(t *testing.T) {
	boom := &stubTool{
		name: "finish",
		execute: func(ctx context.Context, args map[string]any) (ToolResult, error) {
			panic("tool exploded")
		},
	}
	llm := newFakeLLM(sseToolCalls(t, scriptedToolCall{id: "call_1", name: "finish", arguments: "{}"}))
	server := llm.start(t)
	agent := startAgent(t, server, boom)

	msgs, result := collectRun(t, agent, "hi")

	if result.Err == nil || !strings.Contains(result.Err.Error(), "panicked") {
		t.Fatalf("run outcome = %+v (messages: %v)", result, msgTypes(msgs))
	}
	if result.Stopped {
		t.Fatalf("a panic is a failure, not a cancellation: %+v", result)
	}

	// The run slot must be released so the agent can be reused.
	if !agent.beginRun() {
		t.Fatal("run slot was not released after a recovered panic")
	}
	agent.finishRun(RunResult{})
}

func TestLLMFailureIsRetriedAndReported(t *testing.T) {
	llm := newFakeLLM() // empty script: every request fails with HTTP 500
	server := llm.start(t)
	agent := startAgent(t, server)

	_, result := collectRun(t, agent, "hi")

	if result.Err == nil || !strings.Contains(result.Err.Error(), "llm call failed after 3 attempts") {
		t.Fatalf("run outcome = %+v", result)
	}
	if llm.requestCount() != llmMaxAttempts {
		t.Fatalf("llm attempts = %d, want %d", llm.requestCount(), llmMaxAttempts)
	}
}

func TestConcurrentRunIsRejected(t *testing.T) {
	llm := newFakeLLM(fakeLLMBlock)
	server := llm.start(t)
	agent := startAgent(t, server)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel() // unblocks the fake LLM even when the test fails early

	first := startStream(t, agent, ctx, "hi")
	waitForRequest(t, llm, 1)

	if _, err := agent.Run(context.Background(), "again"); err == nil || !strings.Contains(err.Error(), "already running") {
		t.Fatalf("second run error = %v", err)
	}

	agent.Stop()
	drain(t, first)
}

func TestHTTPHeadersAreAttachedToRequests(t *testing.T) {
	llm := newFakeLLM(sseToolCalls(t, scriptedToolCall{id: "call_1", name: "finish", arguments: "{}"}))
	server := llm.start(t)
	agent := startAgent(t, server).WithHTTPHeaders(map[string]string{"X-OpenRouter-Title": "goer-agent-sdk"})

	collectRun(t, agent, "hi")

	if got := llm.requestAt(t, 0).headers.Get("X-OpenRouter-Title"); got != "goer-agent-sdk" {
		t.Fatalf("custom header = %q", got)
	}
}

func TestToolResultTruncationKeepsRunWorking(t *testing.T) {
	big := strings.Repeat("x", 20_000)
	llm := newFakeLLM(sseToolCalls(t, scriptedToolCall{id: "call_1", name: "finish", arguments: "{}"}))
	server := llm.start(t)
	agent := startAgent(t, server, &stubTool{name: "finish", result: ToolResult{Success: true, ModelContent: big}}).
		WithToolResultMaxBytes(1024)

	_, outcome := collectRun(t, agent, "hi")

	if outcome.Err != nil {
		t.Fatalf("run failed: %v", outcome.Err)
	}
	if outcome.Answer != big {
		t.Fatal("the final answer must keep the full end-tool content")
	}
	toolMessage, ok := toolMessageByID(agent.History(), "call_1")
	if !ok {
		t.Fatal("missing tool result")
	}
	if len(toolMessage.Content) > 1024 {
		t.Fatalf("tool result = %d bytes, want <= 1024", len(toolMessage.Content))
	}
	if !strings.Contains(toolMessage.Content, "truncated") {
		t.Fatalf("tool result was not marked as truncated: %q", toolMessage.Content)
	}
}
