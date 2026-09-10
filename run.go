package base

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"

	"github.com/Mrfogg/goer-agent-sdk/ctxkey"
	"github.com/Mrfogg/goer-agent-sdk/xlog"

	"github.com/sashabaranov/go-openai"
)

// Run starts one agent turn and returns the channel of run events. The channel is
// closed after the terminal event: run_done, run_error or run_stopped.
//
// A BaseAgent owns per-run state, so it handles one run at a time. Calling Run
// while a run is active returns a channel carrying a run_error.
//
// The caller is expected to drain the channel until it is closed. Event delivery
// is cancellable, so abandoning the channel cannot block the run goroutine
// forever.
func (a *BaseAgent) Run(ctx context.Context, input string) chan Msg {
	if !a.beginRun() {
		return rejectedRunChannel()
	}

	ctx, cancel := context.WithCancel(ctx)
	msgChan := make(chan Msg, eventChannelBuffer)
	emit := func(event Msg) { dispatchToolEvent(ctx, msgChan, event) }
	emitFinal := func(event Msg) { dispatchFinalEvent(msgChan, event) }

	a.runMu.Lock()
	a.cancelFunc = cancel
	a.runMu.Unlock()

	if a.planModule != nil {
		a.planModule.Reset()
	}

	go func() {
		// Deferred order matters: close(msgChan) runs last, so a panic can still
		// deliver a terminal event, and the run slot is always released.
		defer close(msgChan)
		defer func() {
			a.finishRun()
			if recovered := recover(); recovered != nil {
				agentLogError(ctx, "run panicked: %v\n%s", recovered, debug.Stack())
				emitFinal(Msg{
					Type:    MsgTypeRunError,
					Content: fmt.Sprintf("agent run panicked: %v", recovered),
				})
			}
		}()

		stopHeartbeat := a.startRunHeartbeat(ctx, msgChan)
		defer stopHeartbeat()

		xlog.Info("start run with tools: agent=%s", a.Name())
		answer, err := a.runWithTools(ctx, input, emit)
		if err != nil {
			if isRunStopped(err) || ctx.Err() != nil {
				agentLogInfo(ctx, "run stopped: %v", err)
				emitFinal(Msg{
					Type:    MsgTypeRunStopped,
					Content: stopMessage(a.Lang()),
					Data: map[string]any{
						"reason": err.Error(),
					},
				})
				return
			}

			agentLogError(ctx, "run with tools failed: %v", err)
			emitFinal(Msg{
				Type:    MsgTypeRunError,
				Content: err.Error(),
			})
			return
		}

		emitFinal(Msg{
			Type:    MsgTypeRunDone,
			Content: answer,
			Data: map[string]any{
				"answer": answer,
			},
		})
	}()

	return msgChan
}

// Stop cancels the running turn. The run emits a run_stopped terminal event, so
// callers do not need to emit a separate user-facing stop message.
func (a *BaseAgent) Stop() {
	a.runMu.Lock()
	cancel := a.cancelFunc
	a.runMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func isRunStopped(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (a *BaseAgent) beginRun() bool {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if a.running {
		return false
	}
	a.running = true
	return true
}

func (a *BaseAgent) finishRun() {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	a.running = false
	a.cancelFunc = nil
}

// rejectedRunChannel reports a misused Run call without starting a goroutine.
func rejectedRunChannel() chan Msg {
	msgChan := make(chan Msg, 1)
	msgChan <- Msg{
		Type:    MsgTypeRunError,
		Content: "agent is already running: one BaseAgent instance handles a single run at a time",
	}
	close(msgChan)
	return msgChan
}

// runWithTools drives the tool-calling loop: every iteration is one assistant
// reply, and the run only finishes when an end tool succeeds.
func (a *BaseAgent) runWithTools(ctx context.Context, input string, emit ToolEventEmitter) (string, error) {
	if strings.TrimSpace(a.model) == "" {
		return "", errors.New("model is not configured: pass a model to NewBaseAgent or WithModel")
	}

	var collectedContent []string
	a.addAgentHistory(openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: strings.TrimSpace(input),
		Name:    "original_user_request",
	})
	emit(Msg{Type: MsgTypeStart})
	xlog.Info("user query: %s", input)

	a.runMu.Lock()
	a.memoryBlock = ""
	if a.memoryModule != nil {
		a.memoryBlock = a.memoryModule.BuildPromptBlock(ctx)
	}
	a.runMu.Unlock()

	noToolCallStreak := 0
	for iteration := 1; iteration <= a.maxIterations; iteration++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}

		messages := a.buildMessages(a.systemPrompt)
		logMessageSizes(ctx, messages)

		toolChoice := toolChoiceAuto
		if noToolCallStreak >= noToolCallEscalateAfter {
			toolChoice = toolChoiceRequired
			agentLogInfo(ctx, "no tool call for %d consecutive replies, forcing tool_choice=%s", noToolCallStreak, toolChoiceRequired)
		}

		agentLogInfo(ctx, "start call llm stream: model=%s tool_choice=%s iteration=%d/%d", a.model, toolChoice, iteration, a.maxIterations)
		response, err := a.callLLMWithRetry(ctx, messages, a.model, toolChoice, func(content string) {
			emit(Msg{
				Type:    MsgTypeProgressUpdate,
				Content: content,
			})
		})
		if err != nil {
			return "", err
		}

		assistantMessage := response.Message
		assistantContent := strings.TrimSpace(assistantMessage.Content)
		agentLogInfo(
			ctx,
			"assistant response: content_length=%d content=%q tool_calls=%d tool_details=%s",
			len(assistantContent),
			clipText(assistantContent, logContentMaxRunes),
			len(assistantMessage.ToolCalls),
			formatAssistantToolCallsForLog(assistantMessage.ToolCalls),
		)
		if assistantContent != "" {
			collectedContent = append(collectedContent, assistantContent)
		}
		a.addAgentHistory(assistantMessage)

		if len(assistantMessage.ToolCalls) == 0 {
			noToolCallStreak++
			if noToolCallStreak >= noToolCallFailAfter {
				return "", fmt.Errorf("agent replied without calling a tool %d times in a row; end-tool mode cannot finish this run", noToolCallStreak)
			}
			a.handleNoToolCalls(ctx)
			continue
		}
		noToolCallStreak = 0

		ended, endToolName, endAnswer := a.executeToolCalls(ctx, assistantMessage.ToolCalls, emit)
		if !ended {
			continue
		}

		answer := strings.TrimSpace(endAnswer)
		if answer == "" {
			answer = strings.TrimSpace(strings.Join(collectedContent, "\n\n"))
		}
		if answer == "" {
			errMsg := fmt.Sprintf("end tool %q succeeded but returned no final answer content", endToolName)
			agentLogError(ctx, "%s, adding to context and retrying", errMsg)
			a.addAgentHistory(openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleUser,
				Content: fmt.Sprintf(missingEndToolAnswerTemplate, endToolName, errMsg),
			})
			continue
		}

		return a.finalizeRun(ctx, answer, emit, iteration, fmt.Sprintf("end tool answer: tool=%s", endToolName)), nil
	}

	return "", fmt.Errorf("agent execution exceeded max iterations (%d)", a.maxIterations)
}

// handleNoToolCalls reacts to an assistant turn that produced no tool call. A
// text-only reply can never finish a run in end-tool mode, so a corrective
// message is added and the run continues.
func (a *BaseAgent) handleNoToolCalls(ctx context.Context) {
	endToolsJoined := strings.Join(a.endToolNames(), " or ")
	agentLogError(ctx, "agent returned a text-only reply; it must call one of [%s] to finish, adding to context and retrying", endToolsJoined)
	a.addAgentHistory(openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleUser,
		Content: fmt.Sprintf(noToolCallCorrectionTemplate, endToolsJoined),
	})
}

// finalizeRun emits the answer an end tool produced. Validating the answer is the
// end tool's job: it either returns success with the final content, or it fails
// and the run keeps going. The only thing left here is the optional plan module.
func (a *BaseAgent) finalizeRun(ctx context.Context, answer string, emit ToolEventEmitter, iterations int, endReason string) string {
	answer = strings.TrimSpace(answer)
	if a.planModule != nil {
		planCtx := context.WithValue(ctx, ctxkey.AgentHistory, a.messagesForRequest())
		if err := a.planModule.ValidateFinalAnswer(planCtx); err != nil {
			agentLogInfo(ctx, "plan final validation failed, auto-completing remaining tasks: %v", err)
			if event, ok := a.planModule.CompletePendingTasks(); ok {
				emit(event)
			}
		}
	}

	emit(Msg{
		Type:    MsgTypeMarkdown,
		Content: answer,
	})
	agentLogInfo(ctx, "agent ended with %s: iterations=%d", endReason, iterations)
	return answer
}
