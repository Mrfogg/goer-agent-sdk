// Command tools shows the three things a real tool usually needs: a strict JSON
// schema, structured data for the model, and a product event for the frontend.
//
//	LLM_TOKEN=sk-... LLM_BASE_URL=https://api.openai.com/v1 go run ./examples/tools
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	base "github.com/Mrfogg/goer-agent-sdk"
	"github.com/Mrfogg/goer-agent-sdk/ctxkey"

	"github.com/sashabaranov/go-openai"
	"github.com/sashabaranov/go-openai/jsonschema"
)

const systemPrompt = `You are a sales analyst.
Call query_sales for the numbers, then call finish with a short summary.`

const runTimeout = 2 * time.Minute

// querySalesTool declares a strict schema, returns structured data to the model,
// and emits a chart event for the frontend.
type querySalesTool struct{}

func (querySalesTool) Name() string { return "query_sales" }

func (querySalesTool) Description() string { return "Query sales for a region" }

func (querySalesTool) OpenAIFunctionDefinition() *openai.FunctionDefinition {
	return &openai.FunctionDefinition{
		Name:        "query_sales",
		Description: "Query sales for a region",
		Parameters: base.OpenAIObjectSchema(map[string]jsonschema.Definition{
			"region": base.OpenAIStringSchema("Region name, for example APAC"),
			"months": base.OpenAIIntegerSchema("How many months back to query"),
		}, "region"),
	}
}

func (querySalesTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	region, _ := args["region"].(string)
	series := []int{120, 138, 151, 149, 168}

	// Events reach the product/frontend only; they never enter the model context.
	if emit, ok := ctx.Value(ctxkey.ToolEventEmitter).(base.ToolEventEmitter); ok {
		emit(base.Msg{
			Type: "chart_result",
			Data: map[string]any{
				"chart_id": "sales",
				"chart_option": map[string]any{
					"xAxis": map[string]any{"type": "category", "data": []string{"Apr", "May", "Jun", "Jul", "Aug"}},
					"yAxis": map[string]any{"type": "value"},
					"series": []any{
						map[string]any{"type": "line", "name": region, "data": series},
					},
				},
			},
		})
	}

	return base.ToolResult{
		Success:      true,
		ModelContent: fmt.Sprintf("%s sales for the last 5 months: %v", region, series),
		ModelData: map[string]any{
			"region": region,
			"months": []string{"Apr", "May", "Jun", "Jul", "Aug"},
			"values": series,
		},
	}, nil
}

// finishTool validates the answer before letting the run finish.
type finishTool struct{}

func (finishTool) Name() string { return "finish" }

func (finishTool) Description() string { return "Submit the final summary" }

func (finishTool) OpenAIFunctionDefinition() *openai.FunctionDefinition {
	return &openai.FunctionDefinition{
		Name:        "finish",
		Description: "Submit the final summary",
		Parameters: base.OpenAIObjectSchema(map[string]jsonschema.Definition{
			"answer": base.OpenAIStringSchema("Markdown summary of the findings"),
		}, "answer"),
	}
}

func (finishTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	answer, _ := args["answer"].(string)
	if strings.TrimSpace(answer) == "" {
		return base.ToolResult{Success: false, Error: "answer is required"}, nil
	}
	if len(answer) > 4000 {
		return base.ToolResult{Success: false, Error: "answer is too long, summarise it"}, nil
	}

	// Anything the frontend needs can travel with the message; the model never
	// sees it.
	if emit, ok := ctx.Value(ctxkey.ToolEventEmitter).(base.ToolEventEmitter); ok {
		payload, _ := json.Marshal(map[string]any{"length": len(answer)})
		emit(base.Msg{Type: "task_completed", Content: string(payload)})
	}
	return base.ToolResult{Success: true, ModelContent: answer}, nil
}

func main() {
	agent := base.NewBaseAgent(
		"tools",
		"Tool usage example",
		systemPrompt,
		envOr("LLM_MODEL", "gpt-4o"),
		os.Getenv("LLM_TOKEN"),
		os.Getenv("LLM_BASE_URL"),
		finishTool{},
	)
	agent.AddTool(querySalesTool{})
	agent.WithToolResultMaxBytes(64 * 1024) // keep oversized tool results out of the context

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	stream, err := agent.Run(ctx, "How are APAC sales trending this year?")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	for msg := range stream {
		if msg.Type == base.MsgTypeContent {
			fmt.Print(msg.Content)
		}
	}

	result := agent.Result()
	if result.Err != nil {
		fmt.Fprintln(os.Stderr, "\nerror:", result.Err)
		os.Exit(1)
	}
	fmt.Println("\n\nfinal answer:\n" + result.Answer)
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
