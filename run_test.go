package base

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRunFinishesThroughEndTool(t *testing.T) {
	llm := newFakeLLM(sseToolCalls(t, scriptedToolCall{id: "call_1", name: "finish", arguments: `{"answer":"done"}`}))
	server := llm.start(t)
	agent := startAgent(t, server)

	msgs := collectRun(t, agent, "make a report")

	done, ok := lastMessageOfType(msgs, MsgTypeRunDone)
	if !ok {
		t.Fatalf("expected run_done, got %v", msgTypes(msgs))
	}
	if done.Content != "final answer" {
		t.Fatalf("answer = %q, want %q", done.Content, "final answer")
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

	history := agent.History()
	if _, ok := toolMessageByID(history, "call_1"); !ok {
		t.Fatalf("missing tool result for call_1: %s", describeHistory(history))
	}
}

func TestTextOnlyReplyIsRejectedAndRetried(t *testing.T) {
	llm := newFakeLLM(
		sseText(t, "I would rather just explain the answer."),
		sseToolCalls(t, scriptedToolCall{id: "call_1", name: "finish", arguments: "{}"}),
	)
	server := llm.start(t)
	agent := startAgent(t, server)

	msgs := collectRun(t, agent, "hi")

	if _, ok := lastMessageOfType(msgs, MsgTypeRunDone); !ok {
		t.Fatalf("expected run_done, got %v", msgTypes(msgs))
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

	msgs := collectRun(t, agent, "hi")

	runErr, ok := lastMessageOfType(msgs, MsgTypeRunError)
	if !ok {
		t.Fatalf("expected run_error, got %v", msgTypes(msgs))
	}
	if !strings.Contains(runErr.Content, "without calling a tool") {
		t.Fatalf("unexpected error: %q", runErr.Content)
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

	msgs := collectRun(t, agent, "hi")

	done, ok := lastMessageOfType(msgs, MsgTypeRunDone)
	if !ok || done.Content != "final answer" {
		t.Fatalf("expected the run to continue after a failed end tool, got %v", msgTypes(msgs))
	}
	if llm.requestCount() != 2 {
		t.Fatalf("llm calls = %d, want 2", llm.requestCount())
	}
	history := agent.History()
	failed, ok := toolMessageByID(history, "call_1")
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

	msgs := collectRun(t, agent, "hi")

	if _, ok := lastMessageOfType(msgs, MsgTypeRunDone); !ok {
		t.Fatalf("expected run_done, got %v", msgTypes(msgs))
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

func TestStopEmitsRunStopped(t *testing.T) {
	llm := newFakeLLM(fakeLLMBlock)
	server := llm.start(t)
	agent := startAgent(t, server)

	stream := agent.Run(context.Background(), "hi")
	waitForEvent(t, stream)
	agent.Stop()

	msgs := drain(t, stream)

	stopped, ok := lastMessageOfType(msgs, MsgTypeRunStopped)
	if !ok {
		t.Fatalf("expected run_stopped, got %v", msgTypes(msgs))
	}
	if stopped.Content != "You have stopped the query" {
		t.Fatalf("stop message = %q", stopped.Content)
	}
	if _, ok := lastMessageOfType(msgs, MsgTypeRunDone); ok {
		t.Fatal("a stopped run must not report run_done")
	}
}

func TestToolPanicBecomesRunError(t *testing.T) {
	boom := &stubTool{
		name: "finish",
		execute: func(ctx context.Context, args map[string]any) (ToolResult, error) {
			panic("tool exploded")
		},
	}
	llm := newFakeLLM(sseToolCalls(t, scriptedToolCall{id: "call_1", name: "finish", arguments: "{}"}))
	server := llm.start(t)
	agent := startAgent(t, server, boom)

	msgs := collectRun(t, agent, "hi")

	runErr, ok := lastMessageOfType(msgs, MsgTypeRunError)
	if !ok {
		t.Fatalf("expected run_error, got %v", msgTypes(msgs))
	}
	if !strings.Contains(runErr.Content, "panicked") {
		t.Fatalf("unexpected error: %q", runErr.Content)
	}

	// The run slot must be released so the agent can be reused.
	if !agent.beginRun() {
		t.Fatal("run slot was not released after a recovered panic")
	}
	agent.finishRun()
}

func TestLLMFailureIsRetriedAndReported(t *testing.T) {
	llm := newFakeLLM() // empty script: every request fails with HTTP 500
	server := llm.start(t)
	agent := startAgent(t, server)

	msgs := collectRun(t, agent, "hi")

	runErr, ok := lastMessageOfType(msgs, MsgTypeRunError)
	if !ok {
		t.Fatalf("expected run_error, got %v", msgTypes(msgs))
	}
	if !strings.Contains(runErr.Content, "llm call failed after 3 attempts") {
		t.Fatalf("unexpected error: %q", runErr.Content)
	}
	if llm.requestCount() != llmMaxAttempts {
		t.Fatalf("llm attempts = %d, want %d", llm.requestCount(), llmMaxAttempts)
	}
}

func TestConcurrentRunIsRejected(t *testing.T) {
	llm := newFakeLLM(fakeLLMBlock)
	server := llm.start(t)
	agent := startAgent(t, server)

	first := agent.Run(context.Background(), "hi")
	waitForEvent(t, first)

	second := drain(t, agent.Run(context.Background(), "again"))
	runErr, ok := lastMessageOfType(second, MsgTypeRunError)
	if !ok || !strings.Contains(runErr.Content, "already running") {
		t.Fatalf("second run = %v", msgTypes(second))
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

	msgs := collectRun(t, agent, "hi")

	done, ok := lastMessageOfType(msgs, MsgTypeRunDone)
	if !ok {
		t.Fatalf("expected run_done, got %v", msgTypes(msgs))
	}
	if done.Content != big {
		t.Fatal("the final answer must keep the full end-tool content")
	}
	result, ok := toolMessageByID(agent.History(), "call_1")
	if !ok {
		t.Fatal("missing tool result")
	}
	if len(result.Content) > 1024 {
		t.Fatalf("tool result = %d bytes, want <= 1024", len(result.Content))
	}
	if !strings.Contains(result.Content, "truncated") {
		t.Fatalf("tool result was not marked as truncated: %q", result.Content)
	}
}

func TestToolLanguageMetadataUpdatesAgentLanguage(t *testing.T) {
	langTool := &stubTool{
		name: "finish",
		execute: func(ctx context.Context, args map[string]any) (ToolResult, error) {
			return ToolResult{
				Success:      true,
				ModelContent: "最终答案",
				Meta:         map[string]any{ToolMetaUserQueryLanguageKey: "zh-CN"},
			}, nil
		},
	}
	llm := newFakeLLM(sseToolCalls(t, scriptedToolCall{id: "call_1", name: "finish", arguments: "{}"}))
	server := llm.start(t)
	agent := startAgent(t, server, langTool)

	msgs := collectRun(t, agent, "hi")

	if done, ok := lastMessageOfType(msgs, MsgTypeRunDone); !ok || done.Content != "最终答案" {
		t.Fatalf("expected the localized answer, got %v", msgTypes(msgs))
	}
	if agent.Lang() != "zh-CN" {
		t.Fatalf("agent language = %q, want zh-CN", agent.Lang())
	}
}
