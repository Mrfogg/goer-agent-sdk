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

// RunResult is the outcome of a run. It is available through Result after the
// channel returned by Run has been closed.
type RunResult struct {
	// Answer is the final content the end tool returned.
	Answer string
	// Stopped reports that the run was cancelled: Stop was called, the context was
	// cancelled, or its deadline expired.
	Stopped bool
	// Err is the failure reason when the run did not finish on its own.
	Err error
}

// Run starts one agent turn.
//
// The returned channel carries the model output as it streams: MsgTypeReasoning
// messages for the model's thinking and MsgTypeContent messages for the answer
// text. It is closed when the run ends; read the outcome (final answer, failure
// or cancellation) from Result afterwards.
//
// The error is about the call, not the run: the only case today is calling Run
// while another run is still active, because a BaseAgent owns per-run state and
// therefore handles one run at a time.
//
// Drain the channel, or cancel the context (Stop does this) when you stop
// reading: message delivery is bound to the run context.
func (a *BaseAgent) Run(ctx context.Context, input string) (chan Msg, error) {
	if !a.beginRun() {
		return nil, errors.New("agent is already running: one BaseAgent instance handles a single run at a time")
	}

	ctx, cancel := context.WithCancel(ctx)
	msgChan := make(chan Msg, eventChannelBuffer)
	emit := func(event Msg) { dispatchToolEvent(ctx, msgChan, event) }

	a.runMu.Lock()
	a.cancelFunc = cancel
	a.runMu.Unlock()

	if a.planModule != nil {
		a.planModule.Reset()
	}

	go func() {
		defer close(msgChan)
		var result RunResult
		defer func() {
			if recovered := recover(); recovered != nil {
				agentLogError(ctx, "run panicked: %v\n%s", recovered, debug.Stack())
				result = RunResult{Err: fmt.Errorf("agent run panicked: %v", recovered)}
			}
			a.finishRun(result)
		}()

		xlog.Info("start run with tools: agent=%s", a.Name())
		answer, err := a.runWithTools(ctx, input, emit)
		switch {
		case err == nil:
			result = RunResult{Answer: answer}
		case isRunStopped(err) || ctx.Err() != nil:
			agentLogInfo(ctx, "run stopped: %v", err)
			result = RunResult{Stopped: true, Err: err}
		default:
			agentLogError(ctx, "run with tools failed: %v", err)
			result = RunResult{Err: err}
		}
	}()

	return msgChan, nil
}

// Result returns the outcome of the most recent finished run.
func (a *BaseAgent) Result() RunResult {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	return a.runResult
}

// Stop cancels the running turn. The run ends with RunResult.Stopped set.
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

func (a *BaseAgent) finishRun(result RunResult) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	a.running = false
	a.cancelFunc = nil
	a.runResult = result
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
		response, err := a.callLLMWithRetry(ctx, messages, a.model, toolChoice,
			func(content string) {
				emit(Msg{Type: MsgTypeContent, Content: content})
			},
			func(reasoning string) {
				emit(Msg{Type: MsgTypeReasoning, Content: reasoning})
			},
		)
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

// finalizeRun returns the answer an end tool produced. Validating the answer is
// the end tool's job: it either returns success with the final content, or it
// fails and the run keeps going. The only thing left here is the optional plan
// module.
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

	agentLogInfo(ctx, "agent ended with %s: iterations=%d", endReason, iterations)
	return answer
}
