// Command localmock runs a complete agent turn against a local mock of the
// OpenAI streaming endpoint, so it needs no API key and no network:
//
//	go run ./examples/localmock
//
// The mock answers the three requests of this run on purpose:
//
//  1. a tool call (count_rows)
//  2. thinking text plus a text-only answer, which the runtime rejects because
//     only an end tool may finish a run, so it retries with a corrective message
//  3. a finish tool call, which ends the run
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	base "github.com/Mrfogg/goer-agent-sdk"
)

const systemPrompt = `You are a spreadsheet assistant.
Use count_rows to inspect the sheet, then call finish with the final answer.`

type countRowsTool struct{}

func (countRowsTool) Name() string        { return "count_rows" }
func (countRowsTool) Description() string { return "Count the rows of the current sheet" }

func (countRowsTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	return base.ToolResult{Success: true, ModelContent: "the sheet has 120 rows"}, nil
}

type finishTool struct{}

func (finishTool) Name() string        { return "finish" }
func (finishTool) Description() string { return "Submit the final answer" }

func (finishTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	answer, _ := args["answer"].(string)
	if strings.TrimSpace(answer) == "" {
		return base.ToolResult{Success: false, Error: "answer is required"}, nil
	}
	return base.ToolResult{Success: true, ModelContent: answer}, nil
}

func main() {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}

	server := &http.Server{Handler: &mockLLM{}}
	go func() { _ = server.Serve(listener) }()
	defer func() { _ = server.Close() }()

	baseURL := "http://" + listener.Addr().String()
	fmt.Printf("mock OpenAI endpoint: %s\n\n", baseURL)

	agent := base.NewBaseAgent(
		"localmock",
		"Offline demo agent",
		systemPrompt,
		"mock-model",
		"not-needed", // the mock ignores the token
		baseURL,
		finishTool{},
	)
	agent.AddTool(countRowsTool{})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stream, err := agent.Run(ctx, "How many rows does this sheet have?")
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(1)
	}

	for msg := range stream {
		switch msg.Type {
		case base.MsgTypeReasoning:
			fmt.Printf("[thinking] %s\n", msg.Content)
		case base.MsgTypeContent:
			fmt.Printf("[answer] %s\n", msg.Content)
		}
	}

	result := agent.Result()
	if result.Err != nil {
		fmt.Fprintln(os.Stderr, "error:", result.Err)
		os.Exit(1)
	}
	fmt.Println("\nfinal answer:", result.Answer)
	fmt.Printf("transcript: %d messages\n", len(agent.History()))
}

// mockLLM scripts a streaming OpenAI-compatible endpoint. The reply depends on
// how many requests it has already answered.
type mockLLM struct {
	mu       sync.Mutex
	requests int
}

func (m *mockLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	m.requests++
	index := m.requests
	m.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	switch index {
	case 1:
		writeToolCall(w, "call_rows", "count_rows", "{}")
	case 2:
		writeReasoning(w, "I already know the row count. ")
		writeContent(w, "The sheet has 120 rows.")
	case 3:
		writeToolCall(w, "call_finish", "finish", `{"answer":"The sheet has **120 rows**."}`)
	default:
		writeContent(w, "nothing left to do.")
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writeReasoning(w http.ResponseWriter, text string) {
	writeChunk(w, map[string]any{
		"choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "reasoning_content": text}},
		},
	})
}

func writeContent(w http.ResponseWriter, text string) {
	writeChunk(w, map[string]any{
		"choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": text}},
		},
	})
}

func writeToolCall(w http.ResponseWriter, id, name, arguments string) {
	writeChunk(w, map[string]any{
		"choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"index": 0,
					"id":    id,
					"type":  "function",
					"function": map[string]any{
						"name":      name,
						"arguments": arguments,
					},
				}},
			}},
		},
	})
	writeChunk(w, map[string]any{
		"choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"},
		},
	})
}

func writeChunk(w http.ResponseWriter, payload map[string]any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", encoded)
}
