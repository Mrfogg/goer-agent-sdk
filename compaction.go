package base

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/sashabaranov/go-openai"
)

// Context compaction, part 2: producing the content.
//
// Once the cut point boundary is chosen (see compress.go), the compacted span is
// messages[spanStart:boundary]. Two independent lines produce its output, and the
// results are joined into one role=user compaction block:
//
//   - line A (LLM): renderCompactionSpan renders the span as prefixed plain text,
//     buildSummarizerUserContent folds in the previous summary, and
//     summarizeCompactionSpan calls the summarizer model (no tools,
//     max_tokens=3000) to produce the structured summary_text;
//   - line B (plain code): extractUserMessages pulls every user message of the span
//     out verbatim, and accumulateUserMessages keeps them across compactions under a
//     count cap. This verbatim list never depends on the summarizer model.

const (
	// compactionSummaryMaxTokens caps one summarizer output.
	compactionSummaryMaxTokens = 3000
	// compactionRenderBudgetRunes caps the rune count of the whole rendering; past
	// it, lines are dropped starting from the oldest.
	compactionRenderBudgetRunes = 400000
	// compactionToolResultMaxRunes caps one tool result when rendered (ellipsis
	// included).
	compactionToolResultMaxRunes = 400
	// compactionToolArgsMaxRunes caps one tool call's arguments when rendered
	// (ellipsis included).
	compactionToolArgsMaxRunes = 200
	// compactionUserMsgMaxRunes caps one extracted user message (ellipsis included).
	compactionUserMsgMaxRunes = 600
	// maxCompactedUserMessages caps how many user messages the compaction block keeps
	// verbatim (the newest ones).
	maxCompactedUserMessages = 40
)

// compactionSummarySystemPrompt is the fixed summarizer instruction: 8 fixed
// sections plus three rules.
const compactionSummarySystemPrompt = `You are the sole memory of a tool-calling agent conversation whose full text no longer fits the context window. Everything given to you below is the part being removed. Write a structured summary that lets the agent continue WITHOUT ever seeing the original lines.

Produce exactly these markdown sections, in this order, using these headings:

### 1. Main intent and standing constraints
Restate the user's goal in the user's own phrasing. Standing constraints MUST survive: a constraint outlives the turn that stated it (e.g. "never send anything without approval").

### 2. Key decisions and why
List decisions with their rationale. A decision without a reason invites being re-litigated.

### 3. Files / artifacts involved
Only paths plus their role plus one load-bearing quote. NEVER carry file contents in full.

### 4. Errors and user corrections
"no, do it this way" is the strongest feedback. Record it.

### 5. All user messages
List every user message chronologically. This model-written copy cross-checks against a verbatim extract kept by code.

### 6. Open items / promises / deferred work

### 7. Current work
Be concrete: which step, which file, what state.

### 8. Next actions
The immediate next step implied by the user's intent.

Rules:
- A stale memory of a file is worse than no memory: record that a file was read or edited, and re-read it when needed.
- Be concrete: paths, commands, ids. No vague references.
- Output only the sections above. No preamble, no pleasantries.`

// compactionContinuationContract is the fixed closing text of the compaction
// block; it tells the model how to use this memory.
const compactionContinuationContract = `Continue where you left off: pick up the current work and next step exactly as described. Do not re-ask answered questions, do not recap, do not mention that the context was compacted. If you need the contents of a file noted above, re-read it.`

// foldWhitespace folds every run of whitespace (newlines, tabs, full-width spaces
// included) into a single space and trims the ends.
func foldWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// clipRunes truncates by rune count: past max it keeps the first max-1 runes and
// appends an ellipsis, so the result is exactly max runes long.
func clipRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max-1]) + "…"
}

// renderCompactionSpan renders the compacted span as prefixed lines in
// chronological order:
//   - system / other unknown roles: skipped, the model never sees them;
//   - user: body whitespace folded, never clipped — the user's own words reach the
//     summarizer in full;
//   - assistant: body trimmed and kept on one line; each tool call gets its own line
//     with folded arguments clipped to 200 runes;
//   - tool: result folded and clipped to 400 runes (the first sacrifice: tool
//     results are the largest and the most likely to be stale).
//
// Per-line clipping only stops a single huge item; an overall budget applies after
// it: past 400k runes, lines are dropped starting from the oldest, keeping the
// newest part and prefixing "(…oldest turns elided…)".
func renderCompactionSpan(span []openai.ChatCompletionMessage) string {
	var lines []string
	for _, message := range span {
		switch message.Role {
		case openai.ChatMessageRoleUser:
			if text := foldWhitespace(message.Content); text != "" {
				lines = append(lines, "[user] "+text)
			}
		case openai.ChatMessageRoleAssistant:
			if content := strings.TrimSpace(message.Content); content != "" {
				lines = append(lines, "[assistant] "+content)
			}
			for _, toolCall := range message.ToolCalls {
				name := strings.TrimSpace(toolCall.Function.Name)
				if name == "" {
					continue
				}
				line := "[assistant → " + name + "]"
				if args := clipRunes(foldWhitespace(toolCall.Function.Arguments), compactionToolArgsMaxRunes); args != "" {
					line += " " + args
				}
				lines = append(lines, line)
			}
		case openai.ChatMessageRoleTool:
			if text := clipRunes(foldWhitespace(message.Content), compactionToolResultMaxRunes); text != "" {
				lines = append(lines, "[tool result] "+text)
			}
		}
	}

	total := 0
	keepFrom := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		size := utf8.RuneCountInString(lines[i])
		if total+size > compactionRenderBudgetRunes {
			break
		}
		total += size
		keepFrom = i
	}
	if keepFrom > 0 {
		lines = append([]string{"(…oldest turns elided…)"}, lines[keepFrom:]...)
	}
	return strings.Join(lines, "\n")
}

// buildSummarizerUserContent assembles the user message fed to the summarizer
// model: on a re-compaction the previous summary goes in first as an instruction to
// fold it into the new summary, so the old summary is absorbed along with the span
// and the final compaction block always holds exactly one summary. On the first
// compaction (no previous summary) the rendered text is returned as is.
func buildSummarizerUserContent(prevSummary string, span []openai.ChatCompletionMessage) string {
	rendered := renderCompactionSpan(span)
	prev := strings.TrimSpace(prevSummary)
	if prev == "" {
		return rendered
	}
	var b strings.Builder
	b.WriteString("[previous compaction summary — fold its still-relevant content into the new summary]\n")
	b.WriteString(prev)
	b.WriteString("\n[conversation since]\n")
	b.WriteString(rendered)
	return b.String()
}

// summarizeCompactionSpan calls the summarizer model to produce the structured
// summary. The model is a pure reader here: the request carries only a system
// instruction and a user body, with no tools at all.
func summarizeCompactionSpan(ctx context.Context, client *openai.Client, model string, userContent string) (string, error) {
	if client == nil {
		return "", errors.New("summarize compaction span: nil client")
	}
	request := openai.ChatCompletionRequest{
		Model: model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: compactionSummarySystemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: userContent},
		},
		MaxTokens: compactionSummaryMaxTokens, // output cap, so the summary cannot run away
		// The summarizer holds no tools: Tools / ToolChoice are left unset.
	}
	response, err := client.CreateChatCompletion(ctx, request)
	if err != nil {
		return "", fmt.Errorf("summarize compaction span: %w", err)
	}
	if len(response.Choices) == 0 || strings.TrimSpace(response.Choices[0].Message.Content) == "" {
		return "", errors.New("summarize compaction span: empty summary returned")
	}
	return strings.TrimSpace(response.Choices[0].Message.Content), nil
}

// extractUserMessages pulls every user message of the span out verbatim with plain
// string work and zero LLM calls: whitespace folded, empty messages skipped, single
// messages harder than 600 runes cut (never rewritten).
func extractUserMessages(span []openai.ChatCompletionMessage) []string {
	var userMessages []string
	for _, message := range span {
		if message.Role != openai.ChatMessageRoleUser {
			continue
		}
		if isCompactionBlock(message) {
			// A synthesized compaction block is not a real user message and must not
			// enter the verbatim list.
			continue
		}
		if text := foldWhitespace(message.Content); text != "" {
			userMessages = append(userMessages, clipRunes(text, compactionUserMsgMaxRunes))
		}
	}
	return userMessages
}

// accumulateUserMessages accumulates user messages across compactions: those kept
// by the previous compaction plus the newly extracted ones. Past the 40 message cap
// the oldest are dropped and their count is returned. Dropping is "whole message +
// count" rather than rewriting — the meaning of the dropped oldest messages is
// covered by section 5 of the summary body.
func accumulateUserMessages(prev, current []string) (combined []string, dropped int) {
	combined = append(append([]string{}, prev...), current...)
	if len(combined) > maxCompactedUserMessages {
		dropped = len(combined) - maxCompactedUserMessages
		combined = append([]string{}, combined[len(combined)-maxCompactedUserMessages:]...)
	}
	return combined, dropped
}

// buildCompactionMessage joins the two lines into the compaction block: the summary
// body (line A), the verbatim user message section (line B, omitted entirely when
// empty) and the fixed continuation contract. The message is role=user and, after
// compaction, becomes the prefix of the history: after system and before the
// verbatim tail.
func buildCompactionMessage(summaryText string, userMessages []string, dropped int) openai.ChatCompletionMessage {
	var b strings.Builder
	b.WriteString("<compacted-history>\n")
	b.WriteString(strings.TrimSpace(summaryText))
	if len(userMessages) > 0 || dropped > 0 {
		b.WriteString("\n## User messages in the compacted span (verbatim, chronological)\n")
		if dropped > 0 {
			fmt.Fprintf(&b, "(%d earlier user messages omitted — their intent is covered by the summary above)\n", dropped)
		}
		for _, userMessage := range userMessages {
			b.WriteString("- " + userMessage + "\n")
		}
	}
	b.WriteString("\n" + compactionContinuationContract + "\n")
	b.WriteString("</compacted-history>")
	return openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: b.String(),
	}
}

// compactionState is the state one compaction must carry over to the next, held by
// compressContext:
//   - summary: the previous compaction's summary, folded into the input when
//     compacting again (see buildSummarizerUserContent);
//   - userMessages: the verbatim user message list (accumulated across compactions,
//     under the 40 message cap);
//   - userMsgsDropped: how many user messages the cap has dropped so far
//     (accumulated across compactions).
type compactionState struct {
	summary         string
	userMessages    []string
	userMsgsDropped int
}

// isCompactionBlock reports whether a message is a synthesized block produced by
// compaction (a role=user <compacted-history>). It is used on a re-compaction to
// recognize the previous block at the head of the history so it is not treated as a
// real user message again.
func isCompactionBlock(message openai.ChatCompletionMessage) bool {
	return message.Role == openai.ChatMessageRoleUser &&
		strings.HasPrefix(strings.TrimSpace(message.Content), "<compacted-history>")
}

// planCompaction decides the span of this compaction: span = history[spanStart:boundary].
// (0, 0) means there is nothing new to compact and this compaction is skipped.
//
// The first compaction starts at the head of the history. On a re-compaction
// history[0] is the previous compaction block, so it is skipped and only the growth
// since that compaction is compacted — which structurally guarantees that already
// compacted content is never compacted twice.
//
// Note that this looks at the messages alone (not at compactionState), because the
// history is persisted and restored in a later conversation, where the in-process
// compactionState is gone while the block at the head is still there.
func planCompaction(history []openai.ChatCompletionMessage, keepTokens int) (spanStart, boundary int) {
	boundary = pickCompactionBoundary(history, keepTokens)
	spanStart = 0
	if boundary > 0 && isCompactionBlock(history[0]) {
		spanStart = 1
	}
	if boundary <= spanStart {
		return 0, 0
	}
	return spanStart, boundary
}
