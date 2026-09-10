package base

import "time"

// Runtime defaults. They are named so behavior can be reviewed and tuned in one
// place instead of being scattered as literals.
const (
	// defaultMaxIterations bounds how many agent turns (one LLM reply each) a
	// single run may take.
	defaultMaxIterations = 66

	// eventChannelBuffer is the buffer size of the channel returned by Run.
	eventChannelBuffer = 256

	// logContentMaxRunes caps how much model, tool and argument content is written
	// to logs.
	logContentMaxRunes = 2000

	// defaultToolResultMaxBytes caps the JSON payload of a single tool result
	// message entering the model context. Use WithToolResultMaxBytes to change it,
	// or 0 to disable the cap.
	defaultToolResultMaxBytes = 256 * 1024

	// noToolCallEscalateAfter is the number of consecutive text-only replies after
	// which the agent forces tool_choice=required.
	noToolCallEscalateAfter = 2

	// noToolCallFailAfter is the number of consecutive text-only replies after
	// which the agent gives up and fails the run. End-tool mode never lets a
	// text-only reply finish a run, so this is the escape hatch that keeps a stuck
	// model from burning every iteration.
	noToolCallFailAfter = 5

	// llmMaxAttempts is how often a single LLM call is attempted before the run
	// fails. Transport failures are retried with backoff instead of silently
	// consuming an iteration.
	llmMaxAttempts = 3

	// llmRetryBaseDelay and llmRetryMaxDelay bound the exponential backoff between
	// LLM attempts.
	llmRetryBaseDelay = 500 * time.Millisecond
	llmRetryMaxDelay  = 5 * time.Second

	// toolChoiceAuto lets the model decide whether to call a tool;
	// toolChoiceRequired forces it to call one.
	toolChoiceAuto     = "auto"
	toolChoiceRequired = "required"
)
