package memorykit

import (
	"context"
	"fmt"
	"strings"

	"github.com/Mrfogg/goer-agent-sdk/xlog"
)

// memorySystemPrompt is what belongs in memory and what does not. It is the
// stance, not the mechanics: each tool's own contract (parameters, validation,
// return value) stays in that tool's Description.
//
// The bar here is deliberately aggressive: memory is the durable record of the
// user, so the test for writing is "will this matter in a later session", not
// "is this remarkable".
const memorySystemPrompt = `
MEMORY POLICY
- Memory is the durable record of this user. Default to recording: if a fact about the user, their work, or their requirements could still matter in a later session, write it down. The bar is "will this matter again", not "is this surprising".
- Record at least:
  - Who the user is and what they do: their role, industry, team, and the business they describe.
  - Their preferences and standing instructions: language, format, level of detail, tone, and any explicit "always X / never Y".
  - Requirements, definitions and systems built up across the conversation: business rules, metric definitions, calculation methods, thresholds, taxonomies and naming conventions.
- Skip only what is genuinely transient or re-derivable: small talk, a one-off question, a value already visible in the current message, and credentials or secrets, which are refused outright.
- The memory tools are your only write path to persistent memory; nothing you leave unsaid is remembered.
- Check the known memories list below before recording anything new: if one of them already covers the fact, rewrite that memory with memory_write instead of adding a near-duplicate.
- Keep the store small enough to stay useful: prefer one rich memory over several thin ones.`

// memoryToolPrompt routes between the tools. It only names the tools that are
// actually registered, so the model is never offered an action that is not
// there.
func (m *Module) memoryToolPrompt() string {
	lines := []string{
		"- memory_write(content, memory_id?): record a fact, or rewrite one that is already there. Pass memory_id to replace a memory you can name. The result always tells you whether it saved or rewrote, and what the rewrite replaced.",
	}
	if m != nil && m.merger != nil {
		lines = append(lines,
			"- memory_merge(memory_ids): fold two or more memories that overlap, repeat, or describe the same thing into one. You only choose the ids; the merged text is written for you and keeps every distinct fact.",
		)
	}
	lines = append(lines,
		"- memory_forget(memory_id, user_quote): drop a memory because the user's own words say so — they asked you to forget it, or gave an explicit instruction that makes it obsolete, such as \"don't do X any more\". Quote their words and the server checks them. Never delete on your own initiative: a fact you merely believe is stale or wrong is corrected by rewriting it with memory_write.",
	)
	if m != nil && m.merger != nil {
		lines = append(lines,
			"- Once the list grows long, fold overlapping memories with memory_merge before adding more.",
		)
	}
	return strings.Join(lines, "\n")
}

// memoryCapacityBlock is appended once the store is large enough to need
// compression. It is phrased as a next action rather than a warning, because
// the model is the one that has to act on it.
func memoryCapacityBlock(count int) string {
	return fmt.Sprintf(
		"CAPACITY\n- You are holding %d memories. Before recording anything else, fold overlapping ones with memory_merge so the list stays usable.",
		count,
	)
}

// BuildPromptBlock returns the policy plus the user's known memories. A failed
// load degrades to the policy alone: a missing memory list must not fail a run.
func (m *Module) BuildPromptBlock(ctx context.Context) string {
	prompt := strings.TrimSpace(memorySystemPrompt) + "\n" + m.memoryToolPrompt()

	records, err := m.List(ctx)
	if err != nil {
		xlog.Warn("load memory prompt failed: uid=%d err=%v", m.uid, err)
		return prompt
	}

	if len(records) > 0 {
		prompt += "\n\n" + formatKnownMemories(records)
	}
	if m.merger != nil && len(records) >= m.capacity {
		prompt += "\n\n" + memoryCapacityBlock(len(records))
		xlog.Info("memory store reached capacity hint: uid=%d count=%d", m.uid, len(records))
	}

	xlog.Info("memory prompt block built: uid=%d count=%d", m.uid, len(records))
	return prompt
}

func formatKnownMemories(records []Memory) string {
	var builder strings.Builder
	builder.WriteString("Known memories (from earlier sessions):")
	for _, record := range records {
		builder.WriteString("\n- ")
		builder.WriteString(fmt.Sprintf("%s %s", memoryRef(record), record.Content))
	}
	return builder.String()
}
