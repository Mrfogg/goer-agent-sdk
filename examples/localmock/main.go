// Command localmock runs a complete agent turn against a local mock of the
// OpenAI chat completions endpoint, so it needs no API key and no network:
//
//	go run ./examples/localmock                     # streaming output (default)
//	MODE=non_streaming go run ./examples/localmock  # non-streaming output
//
// The mock answers the three requests of this run on purpose:
//
//  1. a tool call (count_rows)
//  2. thinking text plus a text-only answer, which the runtime rejects because
//     only an end tool may finish a run, so it retries with a corrective message
//  3. a finish tool call, which ends the run
//
// It implements both response shapes too: SSE chunks for streaming and a plain
// JSON completion for non-streaming.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

const runTimeout = 30 * time.Second

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
	fmt.Printf("mock OpenAI endpoint: %s\n", baseURL)

	agent := base.NewBaseAgent(
		"localmock",
		"Offline demo agent",
		systemPrompt,
		"mock-model",
		"not-needed", // the mock ignores the token
		baseURL,
		finishTool{},
	).WithOutputMode(outputModeFromEnv())
	agent.AddTool(countRowsTool{})
	fmt.Printf("output mode: %s\n\n", agent.OutputMode())

	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	stream, err := agent.Run(ctx, "How many rows does this sheet have?")
	if err != nil {
		fmt.Fprintln(os.Stderr, "run:", err)
		os.Exit(1)
	}

	// In streaming mode the model output arrives in many pieces; in
	// non-streaming mode each reply arrives as one message. Either way the last
	// content message is the end tool's answer.
	for msg := range stream {
		switch msg.Type {
		case base.MsgTypeReasoning:
			fmt.Printf("[thinking] %s\n", msg.Content)
		case base.MsgTypeContent:
			fmt.Printf("[content] %s\n", msg.Content)
		case base.MsgTypeUsage:
			fmt.Printf("[usage] input=%v output=%v total=%v\n",
				msg.Data["prompt_tokens"], msg.Data["completion_tokens"], msg.Data["total_tokens"])
		}
	}

	result := agent.Result()
	if result.Err != nil {
		fmt.Fprintln(os.Stderr, "error:", result.Err)
		os.Exit(1)
	}
	fmt.Println("\nresult.Answer:", result.Answer)
	fmt.Printf("run usage: input=%d output=%d total=%d over %d LLM calls\n",
		result.Usage.PromptTokens, result.Usage.CompletionTokens, result.Usage.TotalTokens, result.LLMCalls)
	fmt.Printf("transcript: %d messages\n", len(agent.History()))
}

func outputModeFromEnv() base.OutputMode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MODE"))) {
	case "non_streaming", "non-streaming", "buffer", "buffered":
		return base.OutputModeNonStreaming
	default:
		return base.OutputModeStreaming
	}
}

// mockLLM replies according to how many requests it has already answered, and
// answers with SSE or plain JSON depending on what the client asked for.
type mockLLM struct {
	mu       sync.Mutex
	requests int
}

type mockToolCall struct {
	id        string
	name      string
	arguments string
}

type mockReply struct {
	reasoning string
	content   string
	toolCall  *mockToolCall
}

func replyFor(index int) mockReply {
	switch index {
	case 1:
		return mockReply{toolCall: &mockToolCall{id: "call_rows", name: "count_rows", arguments: "{}"}}
	case 2:
		return mockReply{
			reasoning: "I already know the row count. ",
			content:   "The sheet has 120 rows.",
		}
	case 3:
		return mockReply{toolCall: &mockToolCall{
			id:        "call_finish",
			name:      "finish",
			arguments: `{"answer":"The sheet has **120 rows**."}`,
		}}
	default:
		return mockReply{content: "nothing left to do."}
	}
}

func (m *mockLLM) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var request struct {
		Stream bool `json:"stream"`
	}
	_ = json.Unmarshal(body, &request)

	m.mu.Lock()
	m.requests++
	index := m.requests
	m.mu.Unlock()

	reply := replyFor(index)
	if !request.Stream {
		writeCompletion(w, reply)
		return
	}
	writeStream(w, reply)
}

func writeStream(w http.ResponseWriter, reply mockReply) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	if reply.reasoning != "" {
		writeChunk(w, map[string]any{
			"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "reasoning_content": reply.reasoning}},
			},
		})
	}
	if reply.content != "" {
		writeChunk(w, map[string]any{
			"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{"role": "assistant", "content": reply.content}},
			},
		})
	}
	if reply.toolCall != nil {
		writeChunk(w, map[string]any{
			"choices": []any{
				map[string]any{"index": 0, "delta": map[string]any{
					"role": "assistant",
					"tool_calls": []any{map[string]any{
						"index": 0,
						"id":    reply.toolCall.id,
						"type":  "function",
						"function": map[string]any{
							"name":      reply.toolCall.name,
							"arguments": reply.toolCall.arguments,
						},
					}},
				}},
			},
		})
	}

	finishReason := "stop"
	if reply.toolCall != nil {
		finishReason = "tool_calls"
	}
	// Providers send usage on the final chunk when the request asks for
	// stream_options.include_usage; the agent forwards it as a usage message.
	writeChunk(w, map[string]any{
		"choices": []any{},
		"model":   "mock-model",
		"usage": map[string]any{
			"prompt_tokens":     120,
			"completion_tokens": 24,
			"total_tokens":      144,
		},
	})
	writeChunk(w, map[string]any{
		"choices": []any{
			map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason},
		},
	})
	fmt.Fprint(w, "data: [DONE]\n\n")

	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writeCompletion(w http.ResponseWriter, reply mockReply) {
	message := map[string]any{"role": "assistant"}
	if reply.reasoning != "" {
		message["reasoning_content"] = reply.reasoning
	}
	if reply.content != "" {
		message["content"] = reply.content
	}
	finishReason := "stop"
	if reply.toolCall != nil {
		finishReason = "tool_calls"
		message["tool_calls"] = []any{map[string]any{
			"id":   reply.toolCall.id,
			"type": "function",
			"function": map[string]any{
				"name":      reply.toolCall.name,
				"arguments": reply.toolCall.arguments,
			},
		}}
	}

	payload, err := json.Marshal(map[string]any{
		"id":      "chatcmpl-mock",
		"object":  "chat.completion",
		"created": 0,
		"model":   "mock-model",
		"usage": map[string]any{
			"prompt_tokens":     120,
			"completion_tokens": 24,
			"total_tokens":      144,
		},
		"choices": []any{
			map[string]any{"index": 0, "message": message, "finish_reason": finishReason},
		},
	})
	if err != nil {
		http.Error(w, "mock: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

func writeChunk(w http.ResponseWriter, payload map[string]any) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", encoded)
}
