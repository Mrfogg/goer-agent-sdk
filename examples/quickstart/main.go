// Command quickstart is the smallest useful agent: one regular tool, one end
// tool and a single turn.
//
// Run it against a real model:
//
//	LLM_TOKEN=sk-... LLM_BASE_URL=https://api.openai.com/v1 go run ./examples/quickstart
//
// See examples/localmock for a version that needs no credentials.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	base "github.com/Mrfogg/goer-agent-sdk"
)

const systemPrompt = `You are a spreadsheet analyst.
Use count_rows to inspect the sheet, then call finish with the final answer.`

// Always bound a run: the context deadline is what stops a stuck model or an
// unreachable endpoint.
const runTimeout = 2 * time.Minute

// countRowsTool is a regular tool: the model may call it while working.
type countRowsTool struct{}

func (countRowsTool) Name() string { return "count_rows" }

func (countRowsTool) Description() string { return "Count the rows of the current sheet" }

func (countRowsTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	// A real tool would read the spreadsheet here.
	return base.ToolResult{
		Success:      true,
		ModelContent: "the sheet has 120 rows",
	}, nil
}

// finishTool is the end tool: only a successful call to it finishes the run.
type finishTool struct{}

func (finishTool) Name() string { return "finish" }

func (finishTool) Description() string { return "Submit the final answer when the task is complete" }

func (finishTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	answer, _ := args["answer"].(string)
	if strings.TrimSpace(answer) == "" {
		// A failure keeps the run going: the model sees this error and retries.
		return base.ToolResult{Success: false, Error: "answer is required"}, nil
	}
	return base.ToolResult{Success: true, ModelContent: answer}, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	agent := base.NewBaseAgent(
		"quickstart",
		"Minimal example agent",
		systemPrompt,
		envOr("LLM_MODEL", "gpt-4o"),
		os.Getenv("LLM_TOKEN"),
		os.Getenv("LLM_BASE_URL"),
		finishTool{},
	)
	agent.AddTool(countRowsTool{})

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	stream, err := agent.Run(ctx, "How many rows does this sheet have?")
	if err != nil {
		return err
	}
	for msg := range stream {
		switch msg.Type {
		case base.MsgTypeReasoning:
			fmt.Printf("[thinking] %s\n", msg.Content)
		case base.MsgTypeContent:
			fmt.Printf("[answer] %s\n", msg.Content)
		case base.MsgTypeUsage:
			fmt.Printf("[usage] input=%v output=%v total=%v\n",
				msg.Data["prompt_tokens"], msg.Data["completion_tokens"], msg.Data["total_tokens"])
		}
	}

	result := agent.Result()
	if result.Stopped {
		return fmt.Errorf("run stopped before finishing: %w", result.Err)
	}
	if result.Err != nil {
		return result.Err
	}
	fmt.Println("\nfinal answer:", result.Answer)
	fmt.Printf("tokens: input=%d output=%d over %d LLM calls\n",
		result.Usage.PromptTokens, result.Usage.CompletionTokens, result.LLMCalls)
	return nil
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
