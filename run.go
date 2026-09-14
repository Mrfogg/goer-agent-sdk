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

// RunResult is the outcome of a run. The answer is also emitted on the run
// stream as a MsgTypeContent message; Result exists so a caller can tell the
// three endings apart without tracking messages.
type RunResult struct {
	// Answer is the final content the end tool returned.
	Answer string
	// Stopped reports that the run was cancelled: Stop was called, the context was
	// cancelled, or its deadline expired.
	Stopped bool
	// Err is the failure reason when the run did not finish on its own.
	Err error
	// Usage is the token usage of the run, summed over its LLM calls.
	Usage TokenUsage
	// LLMCalls is how many LLM calls reported usage during the run.
	LLMCalls int
}

// Run starts one agent turn.
//
// The returned channel carries the whole output of the run: MsgTypeReasoning
// messages for the model's thinking, MsgTypeContent messages for the answer
// text, and a final MsgTypeContent message carrying the end tool's answer. The
// channel is closed when the run ends.
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
	a.runUsage = TokenUsage{}
	a.llmCalls = 0
	a.runMu.Unlock()
	a.setRunResult(RunResult{}) // a new run must not expose the previous outcome

	if a.planModule != nil {
		a.planModule.Reset()
	}

	go func() {
		defer close(msgChan)
		defer func() {
			if recovered := recover(); recovered != nil {
				agentLogError(ctx, "run panicked: %v\n%s", recovered, debug.Stack())
				result := RunResult{Err: fmt.Errorf("agent run panicked: %v", recovered)}
				result.Usage, result.LLMCalls = a.runUsageSnapshot()
				a.setRunResult(result)
			}
			a.finishRun()
		}()

		xlog.Info("start run with tools: agent=%s", a.Name())
		a.runWithTools(ctx, input, emit)
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

func (a *BaseAgent) finishRun() {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	a.running = false
	a.cancelFunc = nil
}

func (a *BaseAgent) setRunResult(result RunResult) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	a.runResult = result
}

// addRunUsage accumulates the token usage of one LLM call.
func (a *BaseAgent) addRunUsage(usage TokenUsage) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	a.runUsage = a.runUsage.plus(usage)
	a.llmCalls++
}

func (a *BaseAgent) runUsageSnapshot() (TokenUsage, int) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	return a.runUsage, a.llmCalls
}

// failRun records a failed or cancelled run. Cancellation is reported through
// RunResult.Stopped, everything else through RunResult.Err.
func (a *BaseAgent) failRun(ctx context.Context, err error) {
	usage, calls := a.runUsageSnapshot()
	if isRunStopped(err) || ctx.Err() != nil {
		agentLogInfo(ctx, "run stopped: %v", err)
		a.setRunResult(RunResult{Stopped: true, Err: err, Usage: usage, LLMCalls: calls})
		return
	}
	agentLogError(ctx, "run failed: %v", err)
	a.setRunResult(RunResult{Err: err, Usage: usage, LLMCalls: calls})
}

// maybeCompressContext decides from the prompt_tokens of the most recent LLM call
// (a post-hoc value) whether to compact the context, and runs before every request
// is assembled. It never triggers when no usage is available.
func (a *BaseAgent) maybeCompressContext(ctx context.Context) {
	a.runMu.Lock()
	promptTokens := a.lastPromptTokens
	historyMessages := len(a.agentHistory)
	a.runMu.Unlock()

	if !a.shouldCompressContext(promptTokens) {
		return
	}

	window := a.maxContextTokens
	if window <= 0 {
		window = defaultMaxContextTokens
	}
	agentLogInfo(
		ctx,
		"context compaction triggered: prompt_tokens=%d threshold_tokens=%d max_context_tokens=%d history_messages=%d",
		promptTokens,
		a.contextCompactionThresholdTokens(),
		window,
		historyMessages,
	)
	a.compressContext(ctx)
}

// compressContext compacts a.agentHistory so the next LLM call fits the context
// budget again.
//
// The cut point comes from planCompaction (see compress.go) and the compacted span
// becomes history[spanStart:boundary]. Its output is produced by compaction.go: an
// LLM summary (line A) plus a verbatim user message list (line B), joined into one
// role=user block that replaces the span, while everything after boundary is kept
// verbatim.
//
// Only agentHistory changes — the system prompt is rebuilt by buildMessages on every
// request and never enters the compacted span. Any failure leaves the history
// untouched so the next trigger can retry.
func (a *BaseAgent) compressContext(ctx context.Context) {
	keepTokens := a.contextCompactionKeepTokens()

	// Snapshot under the lock and call the summarizer outside it: the history is only
	// mutated by this run loop, and a concurrent History() reader must not block on a
	// network round trip.
	a.runMu.Lock()
	history := cloneMessages(a.agentHistory)
	spanStart, boundary := planCompaction(history, keepTokens)
	if spanStart == 0 && boundary == 0 {
		a.runMu.Unlock()
		agentLogInfo(ctx, "context compaction skipped: no eligible boundary (keep_tokens=%d)", keepTokens)
		return
	}

	// Line A: fold the previous summary in, then hand the span to the summarizer for
	// a structured summary. When the state is missing (history restored from a store)
	// the previous block at the head is treated as the previous summary, so what it
	// holds still gets folded into the new one.
	prevSummary := ""
	switch {
	case a.compactionState != nil:
		prevSummary = a.compactionState.summary
	case spanStart == 1 && isCompactionBlock(history[0]):
		prevSummary = history[0].Content
	}
	var prevUserMessages []string
	cumulativeDropped := 0
	if a.compactionState != nil {
		prevUserMessages = a.compactionState.userMessages
		cumulativeDropped = a.compactionState.userMsgsDropped
	}
	a.runMu.Unlock()

	span := history[spanStart:boundary]
	userContent := buildSummarizerUserContent(prevSummary, span)
	summary, err := summarizeCompactionSpan(ctx, a.client, a.compactionModel(), userContent)
	if err != nil {
		agentLogError(ctx, "context compaction aborted, history untouched: %v", err)
		return
	}

	// Line B: plain code extracts and accumulates user messages (dropped counts
	// accumulate across compactions too).
	combined, dropped := accumulateUserMessages(prevUserMessages, extractUserMessages(span))
	cumulativeDropped += dropped

	// Replace history[spanStart:boundary] with the block, keeping the tail verbatim.
	block := buildCompactionMessage(summary, combined, cumulativeDropped)
	newHistory := make([]openai.ChatCompletionMessage, 0, 1+len(history)-boundary)
	newHistory = append(newHistory, block)
	newHistory = append(newHistory, history[boundary:]...)

	a.runMu.Lock()
	a.agentHistory = newHistory
	a.compactionState = &compactionState{
		summary:         summary,
		userMessages:    combined,
		userMsgsDropped: cumulativeDropped,
	}
	estimatedTokensAfter := estimateMessagesTokens(newHistory)
	a.runMu.Unlock()

	agentLogInfo(
		ctx,
		"context compaction done: compacted_messages=%d kept_messages=%d boundary=%d estimated_tokens_after=%d",
		boundary-spanStart,
		len(newHistory)-1,
		boundary,
		estimatedTokensAfter,
	)
}

// compactionModel returns the summarizer model. The agent's own model is the only
// source: this SDK parameterizes the model instead of reading it from the
// environment.
func (a *BaseAgent) compactionModel() string {
	return a.model
}

// runWithTools drives the tool-calling loop: every iteration is one assistant
// reply, and the run only finishes when an end tool succeeds.
//
// It deliberately returns nothing: the output of a run is the message stream
// (reasoning/content), and the outcome is recorded on the agent for Result.
func (a *BaseAgent) runWithTools(ctx context.Context, input string, emit ToolEventEmitter) {
	if strings.TrimSpace(a.model) == "" {
		a.failRun(ctx, errors.New("model is not configured: pass a model to NewBaseAgent or WithModel"))
		return
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
			a.failRun(ctx, err)
			return
		}

		// Compaction is checked before the next request is built: it uses the
		// prompt_tokens the previous call reported (a post-hoc value) and rewrites the
		// history about to be sent to the model.
		a.maybeCompressContext(ctx)
		messages := a.buildMessages(a.systemPrompt)
		logMessageSizes(ctx, messages)

		toolChoice := toolChoiceAuto
		if noToolCallStreak >= noToolCallEscalateAfter {
			toolChoice = toolChoiceRequired
			agentLogInfo(ctx, "no tool call for %d consecutive replies, forcing tool_choice=%s", noToolCallStreak, toolChoiceRequired)
		}

		agentLogInfo(ctx, "start call llm (%s): model=%s tool_choice=%s iteration=%d/%d",
			a.outputModeOrDefault(), a.model, toolChoice, iteration, a.maxIterations)
		response, err := a.callLLMWithRetry(ctx, messages, a.model, toolChoice,
			func(content string) {
				emit(Msg{Type: MsgTypeContent, Content: content})
			},
			func(reasoning string) {
				emit(Msg{Type: MsgTypeReasoning, Content: reasoning})
			},
		)
		if err != nil {
			a.failRun(ctx, err)
			return
		}
		if response.Usage != nil {
			emit(usageMessage(*response.Usage))
			a.addRunUsage(*response.Usage)
			a.recordPromptTokens(response.Usage.PromptTokens)
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
				a.failRun(ctx, fmt.Errorf("agent replied without calling a tool %d times in a row; end-tool mode cannot finish this run", noToolCallStreak))
				return
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

		a.finalizeRun(ctx, answer, emit, iteration, fmt.Sprintf("end tool answer: tool=%s", endToolName))
		usage, calls := a.runUsageSnapshot()
		a.setRunResult(RunResult{Answer: answer, Usage: usage, LLMCalls: calls})
		return
	}

	a.failRun(ctx, fmt.Errorf("agent execution exceeded max iterations (%d)", a.maxIterations))
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

// finalizeRun hands the end tool's answer to the caller: it is emitted on the
// stream as a MsgTypeContent message, which is the only place a run's result is
// delivered. Validating the answer is the end tool's job: it either returns
// success with the final content, or it fails and the run keeps going. The only
// thing left here is the optional plan module.
func (a *BaseAgent) finalizeRun(ctx context.Context, answer string, emit ToolEventEmitter, iterations int, endReason string) {
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

	emit(Msg{Type: MsgTypeContent, Content: answer})
	agentLogInfo(ctx, "agent ended with %s: iterations=%d", endReason, iterations)
}
