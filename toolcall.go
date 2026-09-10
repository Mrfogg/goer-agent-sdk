package base

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Mrfogg/goer-agent-sdk/ctxkey"

	"github.com/sashabaranov/go-openai"
)

// executeToolCalls runs the tool calls of one assistant turn in order. When an
// end tool finishes the run, the remaining calls of that turn are recorded as
// skipped so the transcript stays valid for the next request.
func (a *BaseAgent) executeToolCalls(ctx context.Context, toolCalls []openai.ToolCall, emit ToolEventEmitter) (ended bool, endToolName string, answer string) {
	for i, toolCall := range toolCalls {
		ended, answer = a.handleOpenAIToolCall(ctx, toolCall, emit)
		if ended {
			endToolName = strings.TrimSpace(toolCall.Function.Name)
			a.addSkippedToolCallResults(ctx, toolCalls[i+1:], endToolName)
			return true, endToolName, answer
		}
	}
	return false, "", ""
}

// addSkippedToolCallResults appends placeholder tool results for calls the model
// requested but that were never executed, because an end tool already finished the
// run. Without them an assistant message with tool_calls would have no matching
// tool messages, which OpenAI-compatible APIs reject on the next request.
func (a *BaseAgent) addSkippedToolCallResults(ctx context.Context, toolCalls []openai.ToolCall, endToolName string) {
	for _, toolCall := range toolCalls {
		agentLogInfo(ctx, "skipping tool call after end tool %q: tool=%s tool_call_id=%s",
			endToolName, strings.TrimSpace(toolCall.Function.Name), strings.TrimSpace(toolCall.ID))
		result := ToolResult{
			Success: false,
			Error:   fmt.Sprintf("tool call skipped: the run already finished with end tool %q", endToolName),
		}
		a.addAgentHistory(openai.ChatCompletionMessage{
			Role:       openai.ChatMessageRoleTool,
			ToolCallID: toolCall.ID,
			Content:    a.toolResultMessageForHistory(result, nil),
		})
	}
}

func (a *BaseAgent) handleOpenAIToolCall(ctx context.Context, toolCall openai.ToolCall, emit ToolEventEmitter) (ended bool, answer string) {
	toolName := strings.TrimSpace(toolCall.Function.Name)
	if toolName == "" {
		agentLogError(ctx, "tool call failed: tool_call_id=%s error=missing function.name arguments=%s",
			strings.TrimSpace(toolCall.ID), compactJSONForLog(toolCall.Function.Arguments, logContentMaxRunes))
		a.addAgentHistory(openai.ChatCompletionMessage{
			Role:       openai.ChatMessageRoleTool,
			ToolCallID: toolCall.ID,
			Content: a.toolResultMessageForHistory(ToolResult{
				Success: false,
				Error:   "tool call is missing function.name",
			}, nil),
		})
		return false, ""
	}

	args, err := decodeToolCallArguments(toolCall.Function.Arguments)
	if err != nil {
		agentLogError(ctx, "tool call failed: tool=%s tool_call_id=%s error=invalid arguments: %v arguments=%s",
			toolName, strings.TrimSpace(toolCall.ID), err, compactJSONForLog(toolCall.Function.Arguments, logContentMaxRunes))
		a.addAgentHistory(openai.ChatCompletionMessage{
			Role:       openai.ChatMessageRoleTool,
			ToolCallID: toolCall.ID,
			Content: a.toolResultMessageForHistory(ToolResult{
				Success: false,
				Error:   fmt.Sprintf("invalid tool arguments for %s: %v", toolName, err),
			}, nil),
		})
		return false, ""
	}

	result, execErr := a.callTool(ctx, toolName, args, emit)
	historyContent := a.toolResultMessageForHistory(result, execErr)
	agentLogInfo(ctx, "tool call finished: tool=%q tool_call_id=%s arguments_bytes=%d result_bytes=%d result_content_bytes=%d success=%v exec_err=%v",
		toolName, strings.TrimSpace(toolCall.ID), len(toolCall.Function.Arguments), len(historyContent), len(result.ModelContent), result.Success, execErr != nil)
	logToolCallFailure(ctx, toolCall, toolName, result, execErr)

	for _, event := range result.Events {
		emit(event)
	}

	a.addAgentHistory(openai.ChatCompletionMessage{
		Role:       openai.ChatMessageRoleTool,
		ToolCallID: toolCall.ID,
		Content:    historyContent,
	})
	return a.toolCallEndsRun(toolName, result, execErr)
}

func (a *BaseAgent) toolCallEndsRun(toolName string, result ToolResult, execErr error) (ended bool, answer string) {
	if !a.isEndTool(toolName) {
		return false, ""
	}
	if execErr != nil || !result.Success {
		return false, ""
	}
	return true, strings.TrimSpace(result.ModelContent)
}

func decodeToolCallArguments(raw string) (map[string]any, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return map[string]any{}, nil
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(trimmed), &args); err != nil {
		return nil, err
	}
	if args == nil {
		return map[string]any{}, nil
	}
	return args, nil
}

// callTool executes a registered tool with a per-call context carrying the
// current transcript and the event emitter.
func (a *BaseAgent) callTool(ctx context.Context, toolName string, args map[string]any, emit ToolEventEmitter) (ToolResult, error) {
	tool, ok := a.tools[toolName]
	if !ok {
		return ToolResult{}, fmt.Errorf("tool not found: %s", toolName)
	}
	toolCtx := context.WithValue(ctx, ctxkey.AgentHistory, a.messagesForRequest())
	toolCtx = context.WithValue(toolCtx, ctxkey.ToolEventEmitter, emit)
	return tool.Execute(toolCtx, args)
}
