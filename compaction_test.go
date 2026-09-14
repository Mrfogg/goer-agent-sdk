package base

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"
)

func TestRenderCompactionSpan_LineRulesAndOrder(t *testing.T) {
	bigResult := strings.Repeat("x", 600)
	span := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: "sys prompt"},
		{Role: openai.ChatMessageRoleUser, Content: "help me\n\n analyze  sales.csv"},
		{Role: openai.ChatMessageRoleAssistant, Content: "reading the file"},
		{Role: openai.ChatMessageRoleAssistant, ToolCalls: []openai.ToolCall{{
			ID: "call_1", Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{Name: "read_file", Arguments: `{"path":"sales.csv"}`},
		}}},
		{Role: openai.ChatMessageRoleTool, Content: "ok", ToolCallID: "call_1"},
		{Role: openai.ChatMessageRoleTool, Content: bigResult, ToolCallID: "call_1"},
		{Role: "notice", Content: "ui marker"},
		{Role: openai.ChatMessageRoleSystem, Content: "a mid-span system message is skipped too"},
	}

	want := "[user] help me analyze sales.csv\n" + // whitespace folded, kept in full
		"[assistant] reading the file\n" +
		"[assistant → read_file] {\"path\":\"sales.csv\"}\n" +
		"[tool result] ok\n" +
		"[tool result] " + strings.Repeat("x", 399) + "…" // clipped past 400 runes

	if got := renderCompactionSpan(span); got != want {
		t.Fatalf("renderCompactionSpan() =\n%s\nwant:\n%s", got, want)
	}
}

func TestRenderCompactionSpan_OverallBudgetElidesOldest(t *testing.T) {
	// Three user lines of ~150k runes each: 450k in total, past the 400k budget. The
	// oldest one must be dropped (the newest two kept) with "(…oldest turns elided…)"
	// prefixed.
	huge := strings.Repeat("u", 150000)
	span := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: huge + "OLDEST"},
		{Role: openai.ChatMessageRoleUser, Content: huge + "MID"},
		{Role: openai.ChatMessageRoleUser, Content: huge + "NEWEST"},
	}
	got := renderCompactionSpan(span)
	if !strings.HasPrefix(got, "(…oldest turns elided…)\n") {
		t.Fatalf("expected elision marker at head, got prefix: %.80q", got)
	}
	if !strings.Contains(got, "NEWEST") || !strings.Contains(got, "MID") {
		t.Fatalf("expected newest lines kept, got tail: %.160q", got)
	}
	if strings.Contains(got, "OLDEST") {
		t.Fatalf("oldest line should have been elided, got: %.160q", got)
	}
}

func TestBuildSummarizerUserContent(t *testing.T) {
	span := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "requirement A"},
		{Role: openai.ChatMessageRoleAssistant, Content: "A is done"},
	}
	rendered := renderCompactionSpan(span)

	// First compaction: no previous summary, so the rendered text comes back as is.
	if got := buildSummarizerUserContent("", span); got != rendered {
		t.Fatalf("buildSummarizerUserContent(first time) = %q, want %q", got, rendered)
	}

	// Re-compaction: the previous summary goes in first with the instruction to fold it.
	got := buildSummarizerUserContent("OLD SUMMARY", span)
	for _, part := range []string{
		"[previous compaction summary — fold its still-relevant content into the new summary]\nOLD SUMMARY\n[conversation since]\n",
		rendered,
	} {
		if !strings.Contains(got, part) {
			t.Fatalf("buildSummarizerUserContent(re-compaction) missing %q, got:\n%s", part, got)
		}
	}
}

func TestSummarizeCompactionSpan_RequestShapeAndParsing(t *testing.T) {
	var requestBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		requestBody = string(raw)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"  summary text  "},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()

	config := openai.DefaultConfig("test-token")
	config.BaseURL = server.URL + "/v1"
	client := openai.NewClientWithConfig(config)

	got, err := summarizeCompactionSpan(context.Background(), client, "summarizer-model", "user content")
	if err != nil {
		t.Fatalf("summarizeCompactionSpan() error: %v", err)
	}
	if got != "summary text" { // the returned content is trimmed
		t.Fatalf("summarizeCompactionSpan() = %q, want %q", got, "summary text")
	}

	// Request shape: two messages (the system instruction and the user body),
	// max_tokens=3000, and no tools at all.
	for _, want := range []string{
		`"model":"summarizer-model"`,
		`"max_tokens":3000`,
		`"role":"system"`,
		`"role":"user"`,
		"user content",
	} {
		if !strings.Contains(requestBody, want) {
			t.Fatalf("summarize request body missing %s, got: %s", want, requestBody)
		}
	}
	if strings.Contains(requestBody, `"tools"`) {
		t.Fatalf("summarize request must not carry tools, got: %s", requestBody)
	}
}

func TestSummarizeCompactionSpan_EmptyChoicesIsError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[]}`))
	}))
	defer server.Close()

	config := openai.DefaultConfig("test-token")
	config.BaseURL = server.URL + "/v1"
	client := openai.NewClientWithConfig(config)

	if got, err := summarizeCompactionSpan(context.Background(), client, "m", "u"); err == nil || got != "" {
		t.Fatalf("summarizeCompactionSpan() = (%q, %v), want empty result with error", got, err)
	}
}

func TestExtractUserMessages(t *testing.T) {
	long := strings.Repeat("x", 700) // past the 600 cap: 599 runes + "…" = 600
	span := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "  a\n\nb  "},
		{Role: openai.ChatMessageRoleAssistant, Content: "assistant text"},
		{Role: openai.ChatMessageRoleUser, Content: "   "},
		{Role: openai.ChatMessageRoleTool, Content: "tool text", ToolCallID: "call_1"},
		{Role: openai.ChatMessageRoleUser, Content: long},
	}

	got := extractUserMessages(span)
	want := []string{"a b", strings.Repeat("x", 599) + "…"}
	if len(got) != len(want) {
		t.Fatalf("extractUserMessages() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("extractUserMessages()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestExtractUserMessages_SkipsCompactionBlock(t *testing.T) {
	// A synthesized compaction block is not a real user message: even inside the span
	// it must stay out of the verbatim list.
	block := buildCompactionMessage("old summary", []string{"an earlier question"}, 0)
	span := []openai.ChatCompletionMessage{
		block,
		{Role: openai.ChatMessageRoleUser, Content: "a new question"},
	}

	got := extractUserMessages(span)
	want := []string{"a new question"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("extractUserMessages() = %v, want %v", got, want)
	}
}

func TestAccumulateUserMessages_CapDropsOldest(t *testing.T) {
	// Under the cap: concatenated, dropped=0.
	prev := []string{"p1", "p2"}
	cur := []string{"c1"}
	combined, dropped := accumulateUserMessages(prev, cur)
	if len(combined) != 3 || dropped != 0 {
		t.Fatalf("accumulateUserMessages() = (%v, %d), want (3 items, 0)", combined, dropped)
	}

	// Past the cap: 45 + 5 = 50 messages, the oldest 10 dropped, the newest 40 kept.
	prev45 := make([]string, 45)
	for i := range prev45 {
		prev45[i] = "p" + string(rune('A'+i))
	}
	cur5 := []string{"c1", "c2", "c3", "c4", "c5"}
	combined, dropped = accumulateUserMessages(prev45, cur5)
	if dropped != 10 || len(combined) != 40 {
		t.Fatalf("accumulateUserMessages() = (len %d, dropped %d), want (40, 10)", len(combined), dropped)
	}
	if combined[0] != "pK" { // pA..pZ and 45 more, 50 in total, the last 40 kept → first is pK
		t.Fatalf("accumulateUserMessages() oldest kept = %q, want %q", combined[0], "pK")
	}
	if combined[39] != "c5" {
		t.Fatalf("accumulateUserMessages() newest = %q, want %q", combined[39], "c5")
	}
	// The input slices must not be mutated in place.
	if len(prev45) != 45 {
		t.Fatalf("prev slice mutated, len = %d", len(prev45))
	}
}

func TestBuildCompactionMessage(t *testing.T) {
	summary := "### 1. Main intent\nrewrite the file."
	msg := buildCompactionMessage(summary, []string{"help me write a function", "add a retry"}, 2)

	if msg.Role != openai.ChatMessageRoleUser {
		t.Fatalf("compaction message role = %q, want user", msg.Role)
	}
	for _, part := range []string{
		"<compacted-history>\n" + summary,
		"## User messages in the compacted span (verbatim, chronological)",
		"(2 earlier user messages omitted — their intent is covered by the summary above)",
		"- help me write a function\n- add a retry",
		compactionContinuationContract,
		"</compacted-history>",
	} {
		if !strings.Contains(msg.Content, part) {
			t.Fatalf("compaction message missing %q, got:\n%s", part, msg.Content)
		}
	}

	// No user messages: the whole verbatim section (heading included) is absent.
	empty := buildCompactionMessage("S", nil, 0)
	if strings.Contains(empty.Content, "## User messages") {
		t.Fatalf("empty user messages should omit the section, got:\n%s", empty.Content)
	}
	if !strings.Contains(empty.Content, "S\n"+compactionContinuationContract) {
		t.Fatalf("expected summary directly followed by continuation, got:\n%s", empty.Content)
	}
}

func TestIsCompactionBlock(t *testing.T) {
	block := buildCompactionMessage("summary", nil, 0)
	if !isCompactionBlock(block) {
		t.Fatalf("isCompactionBlock(compaction block) = false, want true")
	}
	if isCompactionBlock(openai.ChatCompletionMessage{Role: openai.ChatMessageRoleUser, Content: "normal question"}) {
		t.Fatalf("isCompactionBlock(normal user) = true, want false")
	}
	if isCompactionBlock(openai.ChatCompletionMessage{Role: openai.ChatMessageRoleAssistant, Content: block.Content}) {
		t.Fatalf("isCompactionBlock(assistant with same content) = true, want false")
	}
}

func TestPlanCompaction_FirstTimeStartsAtHead(t *testing.T) {
	huge := strings.Repeat("x", 12000)
	history := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: "read the sheet"},
		{Role: openai.ChatMessageRoleAssistant, Content: "reading"},
		{Role: openai.ChatMessageRoleTool, Content: huge, ToolCallID: "call_1"},
		{Role: openai.ChatMessageRoleUser, Content: "chart"},
		{Role: openai.ChatMessageRoleAssistant, Content: "charted"},
	}
	spanStart, boundary := planCompaction(history, 500)
	if spanStart != 0 || boundary != 3 {
		t.Fatalf("planCompaction(first time) = (%d, %d), want (0, 3)", spanStart, boundary)
	}
}

func TestPlanCompaction_SkipsPreviousBlock(t *testing.T) {
	// Re-compaction: history[0] is the previous block, so it is skipped and only the
	// growth after it is compacted.
	huge := strings.Repeat("x", 12000)
	block := buildCompactionMessage("old summary", nil, 0)
	history := []openai.ChatCompletionMessage{
		block,
		{Role: openai.ChatMessageRoleUser, Content: "change it again"},
		{Role: openai.ChatMessageRoleAssistant, Content: "changed"},
		{Role: openai.ChatMessageRoleTool, Content: huge, ToolCallID: "call_1"},
		{Role: openai.ChatMessageRoleUser, Content: "keep charting"},
		{Role: openai.ChatMessageRoleAssistant, Content: "done"},
	}
	spanStart, boundary := planCompaction(history, 500)
	if spanStart != 1 || boundary != 4 {
		t.Fatalf("planCompaction(re-compaction) = (%d, %d), want (1, 4)", spanStart, boundary)
	}
}

func TestPlanCompaction_RestoredHistorySkipsBlockWithoutState(t *testing.T) {
	// The in-process compactionState is lost when the history is persisted, but the
	// block at the head is still there: it must be recognized from the message itself,
	// otherwise the old block would be compacted a second time as a real user message.
	huge := strings.Repeat("x", 12000)
	block := buildCompactionMessage("old summary", []string{"an earlier question"}, 0)
	history := []openai.ChatCompletionMessage{
		block,
		{Role: openai.ChatMessageRoleUser, Content: "a new question after restore"},
		{Role: openai.ChatMessageRoleAssistant, Content: "working on it"},
		{Role: openai.ChatMessageRoleTool, Content: huge, ToolCallID: "call_1"},
		{Role: openai.ChatMessageRoleUser, Content: "continue"},
		{Role: openai.ChatMessageRoleAssistant, Content: "done"},
	}
	spanStart, boundary := planCompaction(history, 500)
	if spanStart != 1 || boundary != 4 {
		t.Fatalf("planCompaction(restored) = (%d, %d), want (1, 4)", spanStart, boundary)
	}
	if got := extractUserMessages(history[spanStart:boundary]); len(got) != 1 || got[0] != "a new question after restore" {
		t.Fatalf("extractUserMessages(span) = %v, want only the new user message", got)
	}
}

func TestPlanCompaction_NoEligibleBoundary(t *testing.T) {
	// The history is too small: no candidate boundary removes any real content → (0, 0).
	small := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}}
	if spanStart, boundary := planCompaction(small, 500); spanStart != 0 || boundary != 0 {
		t.Fatalf("planCompaction(small, first) = (%d, %d), want (0, 0)", spanStart, boundary)
	}

	// A block exists but nothing of substance grew after it: only the block itself could
	// be cut → still (0, 0).
	block := buildCompactionMessage("S", nil, 0)
	blockOnly := []openai.ChatCompletionMessage{block, {Role: openai.ChatMessageRoleUser, Content: "hi"}}
	if spanStart, boundary := planCompaction(blockOnly, 500); spanStart != 0 || boundary != 0 {
		t.Fatalf("planCompaction(no growth) = (%d, %d), want (0, 0)", spanStart, boundary)
	}
}
