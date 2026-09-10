package base

import (
	"fmt"
	"strings"

	"github.com/sashabaranov/go-openai"
)

// System prompt sections. They live as named templates so prompt policy can be
// reviewed and tuned without touching the run loop.
const (
	// languageRequirementTemplate is prepended when the agent has a target
	// language; %[1]s is the language, %[2]s the rest of the system prompt.
	languageRequirementTemplate = `## Language Requirement (MANDATORY)
You MUST respond in %[1]s language. This is a hard, non-negotiable rule that overrides every other instruction and all context: the source data language, file names, sheet names, column headers, cell values, tool results, and earlier conversation content. Violating it means the task has failed.
Rules you MUST follow:
- Never produce user-visible text in another language, regardless of the language of the source data, file names, headers, or earlier messages. User-facing content always takes precedence over data language.
- When presenting data, translate its labels, headers, and values into %[1]s. Never leave them in the data's original language.
- Stay fully consistent in %[1]s: do not mix languages, do not start in one language and switch later, and do not fall back to English or the data's language.
- If the user's own request is not in %[1]s, still deliver every result in %[1]s.
- Responding in any language other than %[1]s is a failure.

%[2]s`

	// endToolModeTemplate states the termination contract up front, so the model
	// knows a text-only reply cannot finish the run before it ever tries.
	endToolModeTemplate = `END-TOOL MODE (MANDATORY)
- End tools: [%[1]s]. Only a successful call to one of these tools can finish this run; a text-only answer never ends the run and will be sent back to you.
- When the required work is complete, call the appropriate end tool to finish the task. Otherwise, continue working by calling other tools.`

	// noToolCallCorrectionTemplate is injected after a text-only reply.
	noToolCallCorrectionTemplate = `System detected that your previous reply did not call any tool. ` +
		`When the required work is complete, call one of the end tools [%[1]s] to finish the task. ` +
		`Otherwise call another tool to continue working.`

	// missingEndToolAnswerTemplate is injected when an end tool succeeded but
	// produced no final answer content.
	missingEndToolAnswerTemplate = `System detected an anomaly: the end tool %[1]q finished the task but returned no final answer content (%[2]s). ` +
		`Please provide a final answer based on the tool result and existing conversation history.`
)

// buildMessages assembles the full message list for an LLM call: the system
// prompt (language requirement, memory block, end-tool rule) followed by the
// conversation transcript.
func (a *BaseAgent) buildMessages(systemPrompt string) []openai.ChatCompletionMessage {
	if lang := strings.TrimSpace(a.Lang()); lang != "" {
		systemPrompt = fmt.Sprintf(languageRequirementTemplate, lang, systemPrompt)
	}
	if block := strings.TrimSpace(a.memoryPromptBlock()); block != "" {
		systemPrompt += "\n\n" + block
	}
	if section := a.endToolPromptSection(); section != "" {
		systemPrompt += "\n\n" + section
	}

	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleSystem, Content: systemPrompt},
	}
	return append(messages, a.messagesForRequest()...)
}

// endToolPromptSection returns the standing end-tool rule, or an empty string
// when no end tool is registered.
func (a *BaseAgent) endToolPromptSection() string {
	endTools := a.endToolNames()
	if len(endTools) == 0 {
		return ""
	}
	return fmt.Sprintf(endToolModeTemplate, strings.Join(endTools, " or "))
}

// stopMessage returns the user-facing message emitted when a run is stopped. The
// SDK ships with a small built-in set; unknown languages fall back to English.
func stopMessage(lang string) string {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(lang)), "zh") {
		return "您已停止请求"
	}
	return "You have stopped the query"
}
