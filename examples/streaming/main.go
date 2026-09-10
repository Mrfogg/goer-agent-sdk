// Command streaming forwards the model output as it arrives, keeps thinking and
// answer text apart, and shows cancellation: Ctrl+C stops the run, which ends
// with Result().Stopped set.
//
//	LLM_TOKEN=sk-... LLM_BASE_URL=https://api.openai.com/v1 go run ./examples/streaming "summarise this sheet"
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	base "github.com/Mrfogg/goer-agent-sdk"
)

const systemPrompt = `You are a careful analyst. Think first, then call finish with the answer.`

type finishTool struct{}

func (finishTool) Name() string { return "finish" }

func (finishTool) Description() string { return "Submit the final answer" }

func (finishTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	answer, _ := args["answer"].(string)
	if strings.TrimSpace(answer) == "" {
		return base.ToolResult{Success: false, Error: "answer is required"}, nil
	}
	return base.ToolResult{Success: true, ModelContent: answer}, nil
}

func main() {
	question := "Explain what this spreadsheet is about."
	if len(os.Args) > 1 {
		question = strings.Join(os.Args[1:], " ")
	}

	// Ctrl+C (or SIGTERM) cancels the run through the context.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// An optional deadline, so a stuck model cannot run forever.
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	agent := base.NewBaseAgent(
		"streaming",
		"Streaming example",
		systemPrompt,
		envOr("LLM_MODEL", "gpt-4o"),
		os.Getenv("LLM_TOKEN"),
		os.Getenv("LLM_BASE_URL"),
		finishTool{},
	)

	stream, err := agent.Run(ctx, question)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	// Thinking and answer text arrive as separate message types, so a UI can
	// render them in different places (or hide the thinking entirely).
	inReasoning := false
	for msg := range stream {
		switch msg.Type {
		case base.MsgTypeReasoning:
			if !inReasoning {
				fmt.Print("\n--- thinking ---\n")
				inReasoning = true
			}
			fmt.Print(msg.Content)
		case base.MsgTypeContent:
			if inReasoning {
				fmt.Print("\n--- answer ---\n")
				inReasoning = false
			}
			fmt.Print(msg.Content)
		case base.MsgTypeUsage:
			fmt.Fprintf(os.Stderr, "\n[usage] input=%v output=%v total=%v\n",
				msg.Data["prompt_tokens"], msg.Data["completion_tokens"], msg.Data["total_tokens"])
		}
	}

	result := agent.Result()
	switch {
	case result.Err != nil && result.Stopped:
		fmt.Printf("\n\nstopped: %v\n", result.Err)
	case result.Err != nil:
		fmt.Fprintf(os.Stderr, "\n\nerror: %v\n", result.Err)
		os.Exit(1)
	default:
		fmt.Printf("\n\nfinal answer:\n%s\n", result.Answer)
		fmt.Printf("tokens: input=%d output=%d over %d LLM calls\n",
			result.Usage.PromptTokens, result.Usage.CompletionTokens, result.LLMCalls)
	}
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
