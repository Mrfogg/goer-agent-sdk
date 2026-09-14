package base

import (
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"
)

// routeATestMessages builds a history:
//
//	0 system
//	1 user  "analyze sales.csv"
//	2 assistant
//	3 tool   header preview (small)
//	4 user  "clean it before charting"
//	5 assistant
//	6 tool   huge result (nothing large after index 6)
//	7 user  "switch the X axis to months"
//	8 assistant
//	9 user  "package and run"
//	10 assistant
//
// The huge result sits at index 6, which makes suffix(4) over budget while suffix(7)
// fits → the boundary must be 7.
func routeATestMessages() []openai.ChatCompletionMessage {
	huge := strings.Repeat("x", 12000) // roughly 3000+ tokens by the estimate
	return []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "you are a data analyst"},
		{Role: openai.ChatMessageRoleUser, Content: "analyze sales.csv"},
		{Role: openai.ChatMessageRoleAssistant, Content: "reading the file first"},
		{Role: openai.ChatMessageRoleTool, Content: `{"success":true,"content":"columns:..."}`, ToolCallID: "call_1"},
		{Role: openai.ChatMessageRoleUser, Content: "clean it before charting"},
		{Role: openai.ChatMessageRoleAssistant, Content: "ok, writing a cleaning script"},
		{Role: openai.ChatMessageRoleTool, Content: huge, ToolCallID: "call_2"},
		{Role: openai.ChatMessageRoleUser, Content: "switch the X axis to months"},
		{Role: openai.ChatMessageRoleAssistant, Content: "chart config updated"},
		{Role: openai.ChatMessageRoleUser, Content: "package and run"},
		{Role: openai.ChatMessageRoleAssistant, Content: "run finished"},
	}
}

func TestPickCompactionBoundary_RouteA_CutsAtEarliestUserTurnThatFits(t *testing.T) {
	messages := routeATestMessages()
	if got := pickCompactionBoundary(messages, 500); got != 7 {
		t.Fatalf("pickCompactionBoundary() = %d, want 7", got)
	}
}

func TestPickCompactionBoundary_RouteA_TakesEarliestFitNotNewest(t *testing.T) {
	// The large block sits at index 3: suffix(1) is over budget while suffix(4) fits →
	// the cut must land on 4 (the first user that fits), not on a later 7.
	huge := strings.Repeat("x", 12000)
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleUser, Content: "read the sheet"},
		{Role: openai.ChatMessageRoleAssistant, Content: "reading"},
		{Role: openai.ChatMessageRoleTool, Content: huge, ToolCallID: "call_1"},
		{Role: openai.ChatMessageRoleUser, Content: "clean"},
		{Role: openai.ChatMessageRoleAssistant, Content: "cleaned"},
		{Role: openai.ChatMessageRoleUser, Content: "chart"},
		{Role: openai.ChatMessageRoleAssistant, Content: "charted"},
	}
	if got := pickCompactionBoundary(messages, 500); got != 4 {
		t.Fatalf("pickCompactionBoundary() = %d, want 4", got)
	}
}

func TestPickCompactionBoundary_RouteB_CutsInsideNewestUserTurn(t *testing.T) {
	// The newest turn (starting at user 1) holds a huge tool result (index 3), so even
	// the suffix from user 1 is over budget. Assistant iteration points inside the turn:
	// 2, 4, 6 → suffix(2) is over budget, suffix(4) fits → the boundary must be 4.
	huge := strings.Repeat("x", 12000)
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleUser, Content: "read the big file"},
		{Role: openai.ChatMessageRoleAssistant, Content: "starting the read"},
		{Role: openai.ChatMessageRoleTool, Content: huge, ToolCallID: "call_1"},
		{Role: openai.ChatMessageRoleAssistant, Content: "read, now cleaning"},
		{Role: openai.ChatMessageRoleTool, Content: `{"success":true}`, ToolCallID: "call_2"},
		{Role: openai.ChatMessageRoleAssistant, Content: "cleaning done"},
	}
	if got := pickCompactionBoundary(messages, 500); got != 4 {
		t.Fatalf("pickCompactionBoundary() = %d, want 4", got)
	}
}

func TestPickCompactionBoundary_RouteB_DegradesToNewestAssistant(t *testing.T) {
	// keep is tiny: no assistant suffix inside the turn fits → degrade to the last
	// assistant of the newest user turn.
	huge := strings.Repeat("x", 12000)
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleUser, Content: "read the big file"},
		{Role: openai.ChatMessageRoleAssistant, Content: "starting the read"},
		{Role: openai.ChatMessageRoleTool, Content: huge, ToolCallID: "call_1"},
		{Role: openai.ChatMessageRoleAssistant, Content: "read"},
		{Role: openai.ChatMessageRoleTool, Content: `{"success":true}`, ToolCallID: "call_2"},
		{Role: openai.ChatMessageRoleAssistant, Content: "cleaning done"},
	}
	if got := pickCompactionBoundary(messages, 1); got != 6 {
		t.Fatalf("pickCompactionBoundary() = %d, want 6", got)
	}
}

func TestPickCompactionBoundary_RouteC_NoUserMessages(t *testing.T) {
	// Tool-only conversation (no user message): the large tool result sits at index 2,
	// suffix(1) is over budget and suffix(3) fits → cut between assistant iteration
	// points at 3.
	medium := strings.Repeat("x", 600) // roughly 150 tokens by the estimate
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleAssistant, Content: "step one"},
		{Role: openai.ChatMessageRoleTool, Content: medium, ToolCallID: "call_1"},
		{Role: openai.ChatMessageRoleAssistant, Content: "step two"},
		{Role: openai.ChatMessageRoleTool, Content: "ok", ToolCallID: "call_2"},
		{Role: openai.ChatMessageRoleAssistant, Content: "done"},
	}
	if got := pickCompactionBoundary(messages, 80); got != 3 {
		t.Fatalf("pickCompactionBoundary() = %d, want 3", got)
	}
}

func TestPickCompactionBoundary_NoLegalCandidate(t *testing.T) {
	if got := pickCompactionBoundary(nil, 500); got != -1 {
		t.Fatalf("pickCompactionBoundary(empty) = %d, want -1", got)
	}
	onlySystem := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: "system"}}
	if got := pickCompactionBoundary(onlySystem, 500); got != -1 {
		t.Fatalf("pickCompactionBoundary(system only) = %d, want -1", got)
	}
}

func TestPickCompactionBoundary_OnlySystemAndOneUser(t *testing.T) {
	// Only system + one user: the user suffix fits → returns 1. That boundary equals
	// start, so the caller's guard (skip when b <= start) has to reject it.
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "system"},
		{Role: openai.ChatMessageRoleUser, Content: "hi"},
	}
	if got := pickCompactionBoundary(messages, 500); got != 1 {
		t.Fatalf("pickCompactionBoundary() = %d, want 1", got)
	}
}
