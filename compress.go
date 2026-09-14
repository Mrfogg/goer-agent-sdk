package base

import (
	"encoding/json"

	"github.com/sashabaranov/go-openai"
)

// Context compaction, part 1: choosing the cut point (boundary).
//
// One compaction step picks a single index, boundary:
//
//   - messages[:boundary] goes into the compaction block (summarized, no longer
//     present verbatim);
//   - messages[boundary:] is kept verbatim.
//
// The only input is the token budget keepTokens: messages[boundary:] must fit in
// it. Two constraints: system never enters the compacted span, and a tool message
// can never be the boundary — cutting on a tool message would hand the model a
// tool result whose call is gone.

// estimateMessagesTokens heuristically estimates the token count of a message
// list: every message is measured by its serialized (JSON) size, at roughly
// 4 characters per token. It only drives the cut decision, where the suffix has to
// shrink monotonically with the index, so it does not need to be accurate.
func estimateMessagesTokens(messages []openai.ChatCompletionMessage) int {
	total := 0
	for _, message := range messages {
		if bs, err := json.Marshal(message); err == nil {
			total += len(bs)
		}
	}
	return total / 4
}

// compactionStartIndex returns the index of the first real message (skipping the
// leading system message).
func compactionStartIndex(messages []openai.ChatCompletionMessage) int {
	if len(messages) > 0 && messages[0].Role == openai.ChatMessageRoleSystem {
		return 1
	}
	return 0
}

// collectCompactionCandidates collects every index that may serve as the
// boundary. user is preferred (a turn starts there, so cutting keeps the turn
// intact); assistant is the fallback (an iteration start, which splits a turn and
// is only used when there is no other way). tool / system and other roles are
// never candidates.
func collectCompactionCandidates(messages []openai.ChatCompletionMessage, start int) (users, assistants []int) {
	for i := start; i < len(messages); i++ {
		switch messages[i].Role {
		case openai.ChatMessageRoleUser:
			users = append(users, i)
		case openai.ChatMessageRoleAssistant:
			assistants = append(assistants, i)
		}
	}
	return users, assistants
}

// fitCompactionBoundary returns the first index in candidates (ascending) whose
// suffix fits the budget, or -1 when none does.
//
// The first hit is also the best one: the suffix shrinks monotonically as the
// index grows, so the first index that fits keeps the most verbatim content.
func fitCompactionBoundary(candidates []int, messages []openai.ChatCompletionMessage, keepTokens int) int {
	for _, i := range candidates {
		if estimateMessagesTokens(messages[i:]) <= keepTokens {
			return i
		}
	}
	return -1
}

// pickCompactionBoundary picks the compaction boundary with three fallbacks:
//   - route A (normal): search the user (turn start) boundaries first;
//   - route B (the newest user turn alone blows the budget): step inside that
//     turn and search the assistant iteration points after it, keeping at least
//     the most recent assistant step;
//   - route C (last resort, tool-only conversations with no user message):
//     search between the assistant iteration points.
//
// The caller still has two guards to apply, otherwise this compaction is skipped:
//  1. the boundary must land after the first real message (b < 0 || b <= start);
//  2. across repeated compactions the boundary must move strictly rightwards (never
//     compact what was already compacted).
func pickCompactionBoundary(messages []openai.ChatCompletionMessage, keepTokens int) int {
	start := compactionStartIndex(messages)
	users, assistants := collectCompactionCandidates(messages, start)

	// Route A.
	if b := fitCompactionBoundary(users, messages, keepTokens); b >= 0 {
		return b
	}

	// Route B: route A failed, so every suffix starting at the newest user is over
	// budget — the newest turn alone blows it (typically a huge tool result inside
	// one turn).
	if len(users) > 0 {
		lastUser := users[len(users)-1]
		inside := make([]int, 0, len(assistants))
		for _, a := range assistants {
			if a > lastUser {
				inside = append(inside, a)
			}
		}
		if b := fitCompactionBoundary(inside, messages, keepTokens); b >= 0 {
			return b
		}
		if len(inside) > 0 {
			return inside[len(inside)-1] // degrade: the last assistant of the newest turn
		}
		return lastUser // degrade further: the newest user message
	}

	// Route C.
	if b := fitCompactionBoundary(assistants, messages, keepTokens); b >= 0 {
		return b
	}
	if len(assistants) > 0 {
		return assistants[len(assistants)-1]
	}

	return -1 // no legal candidate at all
}
