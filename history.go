package base

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/sashabaranov/go-openai"
)

// WithHistory seeds the agent with a previous conversation. The slice is copied,
// so later mutations on either side do not leak into the other.
//
// Persisting history across turns is the caller's responsibility: load it, pass
// it here, and read the updated transcript back through History after the run.
func (a *BaseAgent) WithHistory(history []openai.ChatCompletionMessage) *BaseAgent {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	a.agentHistory = cloneMessages(history)
	return a
}

// History returns a copy of the current conversation transcript.
func (a *BaseAgent) History() []openai.ChatCompletionMessage {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	return cloneMessages(a.agentHistory)
}

// HasState reports whether the agent already holds conversation history.
func (a *BaseAgent) HasState() bool {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	return len(a.agentHistory) > 0
}

// messagesForRequest returns the transcript to send to the model.
func (a *BaseAgent) messagesForRequest() []openai.ChatCompletionMessage {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	return cloneMessages(a.agentHistory)
}

func (a *BaseAgent) addAgentHistory(msg openai.ChatCompletionMessage) *BaseAgent {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	a.agentHistory = append(a.agentHistory, msg)
	return a
}

// toolResultMessageForHistory serializes a tool result into the JSON payload that
// enters the model context, truncating it when it exceeds the configured budget.
func (a *BaseAgent) toolResultMessageForHistory(result ToolResult, execErr error) string {
	encoded, err := json.Marshal(toolResultPayload(result, execErr))
	if err != nil {
		return toolResultEncodingFailure(result, execErr)
	}

	limit := a.toolResultMaxBytes
	if limit <= 0 || len(encoded) <= limit {
		return string(encoded)
	}

	// Context budget exceeded: keep the status plus a clipped rendering of the
	// payload, so the model knows the result was dropped instead of empty.
	clipped := map[string]any{
		"success":   result.Success,
		"truncated": true,
		"content":   clipText(string(encoded), limit/4),
		"note":      fmt.Sprintf("tool result truncated: %d bytes exceeded the %d byte limit", len(encoded), limit),
	}
	if errText := toolResultErrorText(result, execErr); errText != "" {
		clipped["error"] = errText
	}
	clippedEncoded, clipErr := json.Marshal(clipped)
	if clipErr != nil || len(clippedEncoded) > limit {
		return fmt.Sprintf(`{"success":%t,"truncated":true,"error":"tool result exceeded the %d byte limit"}`, result.Success, limit)
	}
	return string(clippedEncoded)
}

func toolResultPayload(result ToolResult, execErr error) map[string]any {
	payload := map[string]any{
		"success": result.Success,
		"content": result.ModelContent,
		"data":    result.ModelData,
		"meta":    result.Meta,
	}
	if errText := toolResultErrorText(result, execErr); errText != "" {
		payload["error"] = errText
	}
	return payload
}

func toolResultErrorText(result ToolResult, execErr error) string {
	if execErr != nil {
		return execErr.Error()
	}
	return strings.TrimSpace(result.Error)
}

func toolResultEncodingFailure(result ToolResult, execErr error) string {
	if errText := toolResultErrorText(result, execErr); errText != "" {
		return fmt.Sprintf(`{"success":false,"error":%q}`, errText)
	}
	return `{"success":false,"error":"failed to encode tool result"}`
}

// cloneMessages copies the transcript, including each message's nested slices and
// pointers, so agent and caller never share mutable state.
func cloneMessages(history []openai.ChatCompletionMessage) []openai.ChatCompletionMessage {
	if len(history) == 0 {
		return nil
	}
	cloned := make([]openai.ChatCompletionMessage, len(history))
	for i, msg := range history {
		cloned[i] = cloneMessage(msg)
	}
	return cloned
}

func cloneMessage(msg openai.ChatCompletionMessage) openai.ChatCompletionMessage {
	cloned := msg
	if len(msg.ToolCalls) > 0 {
		cloned.ToolCalls = append([]openai.ToolCall(nil), msg.ToolCalls...)
	}
	if len(msg.MultiContent) > 0 {
		cloned.MultiContent = append([]openai.ChatMessagePart(nil), msg.MultiContent...)
	}
	if msg.FunctionCall != nil {
		functionCall := *msg.FunctionCall
		cloned.FunctionCall = &functionCall
	}
	return cloned
}

func toolResultUserQueryLanguage(result ToolResult) string {
	raw, ok := result.Meta[ToolMetaUserQueryLanguageKey].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(raw)
}
