// Command multiturn is a terminal REPL over one conversation. History lives in
// memory here; a real service would load and save it per chat.
//
//	LLM_TOKEN=sk-... LLM_BASE_URL=https://api.openai.com/v1 go run ./examples/multiturn
//
// Commands: /history prints the transcript size, /reset forgets it, /exit quits.
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	base "github.com/Mrfogg/goer-agent-sdk"

	"github.com/sashabaranov/go-openai"
)

const systemPrompt = `You are a helpful spreadsheet assistant.
Answer questions with the tools, then call finish with the final answer.`

const runTimeout = 2 * time.Minute

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
	// One instance per turn, seeded with the history of the previous ones.
	var history []openai.ChatCompletionMessage

	in := bufio.NewScanner(os.Stdin)
	fmt.Println("Ask something (/history, /reset, /exit):")

	for {
		fmt.Print("> ")
		if !in.Scan() {
			return
		}
		input := strings.TrimSpace(in.Text())
		switch input {
		case "":
			continue
		case "/exit":
			return
		case "/history":
			fmt.Printf("%d messages in history\n", len(history))
			continue
		case "/reset":
			history = nil
			fmt.Println("history cleared")
			continue
		}

		answer, updated, err := ask(input, history)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			continue
		}
		history = updated
		fmt.Println(answer)
	}
}

func ask(input string, history []openai.ChatCompletionMessage) (string, []openai.ChatCompletionMessage, error) {
	agent := base.NewBaseAgent(
		"multiturn",
		"Multi-turn example",
		systemPrompt,
		envOr("LLM_MODEL", "gpt-4o"),
		os.Getenv("LLM_TOKEN"),
		os.Getenv("LLM_BASE_URL"),
		finishTool{},
	).WithHistory(history)

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	stream, err := agent.Run(ctx, input)
	if err != nil {
		return "", history, err
	}
	for range stream {
		// Nothing to stream here: the REPL prints the final answer only.
	}

	result := agent.Result()
	if result.Err != nil {
		return "", agent.History(), result.Err
	}
	return result.Answer, agent.History(), nil
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
