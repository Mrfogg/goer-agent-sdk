package base

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/excelmatic/goer-agent-sdk/ctxkey"
	"github.com/excelmatic/goer-agent-sdk/xlog"

	"github.com/sashabaranov/go-openai"
)

func agentLogDebug(ctx context.Context, format string, args ...any) {
	xlog.Debug("chat_id=%s "+format, append([]any{chatIDFromContext(ctx)}, args...)...)
}

func agentLogInfo(ctx context.Context, format string, args ...any) {
	xlog.Info("chat_id=%s "+format, append([]any{chatIDFromContext(ctx)}, args...)...)
}

func agentLogWarn(ctx context.Context, format string, args ...any) {
	xlog.Warn("chat_id=%s "+format, append([]any{chatIDFromContext(ctx)}, args...)...)
}

func agentLogError(ctx context.Context, format string, args ...any) {
	xlog.Error("chat_id=%s "+format, append([]any{chatIDFromContext(ctx)}, args...)...)
}

func chatIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}

	chatID := strings.TrimSpace(fmt.Sprint(ctx.Value(ctxkey.ChatID)))
	if chatID == "" || chatID == "<nil>" {
		return ""
	}
	return chatID
}

// logMessageSizes records the size breakdown of the assembled LLM messages so
// context overflow can be diagnosed: total chars, estimated tokens, and each
// message's role, name, and content length.
func logMessageSizes(ctx context.Context, messages []openai.ChatCompletionMessage) {
	totalChars := 0
	var b strings.Builder
	b.WriteString(fmt.Sprintf("llm request messages: count=%d", len(messages)))
	for i, m := range messages {
		contentLen := len(m.Content)
		totalChars += contentLen
		b.WriteString(fmt.Sprintf(", [%d]role=%s name=%q len=%d", i, m.Role, m.Name, contentLen))
	}
	b.WriteString(fmt.Sprintf(", total_chars=%d estimated_tokens=%d", totalChars, totalChars/4))
	agentLogDebug(ctx, b.String())
}

func formatAssistantToolCallsForLog(toolCalls []openai.ToolCall) string {
	if len(toolCalls) == 0 {
		return "[]"
	}

	type toolCallLogEntry struct {
		ID        string `json:"id,omitempty"`
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	}

	entries := make([]toolCallLogEntry, 0, len(toolCalls))
	for _, toolCall := range toolCalls {
		entries = append(entries, toolCallLogEntry{
			ID:        strings.TrimSpace(toolCall.ID),
			Name:      strings.TrimSpace(toolCall.Function.Name),
			Arguments: compactJSONForLog(toolCall.Function.Arguments, logContentMaxRunes),
		})
	}

	bs, err := json.Marshal(entries)
	if err != nil {
		return fmt.Sprintf("%+v", entries)
	}
	return string(bs)
}

func logToolCallFailure(ctx context.Context, toolCall openai.ToolCall, toolName string, result ToolResult, execErr error) {
	if execErr == nil && result.Success {
		return
	}

	toolCallID := strings.TrimSpace(toolCall.ID)
	arguments := compactJSONForLog(toolCall.Function.Arguments, logContentMaxRunes)

	if execErr != nil {
		agentLogError(ctx, "tool call failed: tool=%s tool_call_id=%s arguments=%s error=%v", toolName, toolCallID, arguments, execErr)
		return
	}

	errMsg := strings.TrimSpace(result.Error)
	if errMsg == "" {
		errMsg = "tool returned success=false without an error message"
	}
	agentLogError(ctx, "tool call returned unsuccessful result: tool=%s tool_call_id=%s arguments=%s error=%s", toolName, toolCallID, arguments, errMsg)
}

// compactJSONForLog renders raw JSON on a single line, clipped for logs.
func compactJSONForLog(raw string, maxRunes int) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}

	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(trimmed)); err == nil {
		trimmed = compact.String()
	}

	return clipText(trimmed, maxRunes)
}

// clipText truncates content to maxRunes runes. A non-positive maxRunes means
// "no limit".
func clipText(content string, maxRunes int) string {
	if maxRunes <= 0 || utf8.RuneCountInString(content) <= maxRunes {
		return content
	}
	runes := []rune(content)
	return strings.TrimSpace(string(runes[:maxRunes])) + "...(truncated)"
}
