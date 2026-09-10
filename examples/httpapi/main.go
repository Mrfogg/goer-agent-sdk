// Command httpapi serves the agent over HTTP. Each request builds its own agent
// (one instance handles one run) and streams newline-delimited JSON messages
// until the run ends, followed by a single result line.
//
//	LLM_TOKEN=sk-... LLM_BASE_URL=https://api.openai.com/v1 go run ./examples/httpapi
//	curl -N -X POST localhost:8080/ask -d '{"input":"How many rows does this sheet have?"}'
//
// Because the run uses r.Context(), closing the client connection cancels the
// run, which is what a "stop" button should do.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	base "github.com/Mrfogg/goer-agent-sdk"
)

const (
	systemPrompt  = `You are a spreadsheet assistant. Use the tools, then call finish with the final answer.`
	runTimeout    = 5 * time.Minute
	listenAddress = ":8080"
)

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

type askRequest struct {
	Input string `json:"input"`
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/ask", handleAsk)

	log.Printf("listening on %s (POST /ask)", listenAddress)
	if err := http.ListenAndServe(listenAddress, mux); err != nil {
		log.Fatal(err)
	}
}

func handleAsk(w http.ResponseWriter, r *http.Request) {
	var request askRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(request.Input) == "" {
		http.Error(w, "input is required", http.StatusBadRequest)
		return
	}

	flusher, canFlush := w.(http.Flusher)
	w.Header().Set("Content-Type", "application/x-ndjson")
	encoder := json.NewEncoder(w)

	// One agent per request: no shared state, and r.Context() cancels the run when
	// the client disconnects.
	ctx, cancel := context.WithTimeout(r.Context(), runTimeout)
	defer cancel()

	agent := newAgent()
	stream, err := agent.Run(ctx, request.Input)
	if err != nil {
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	for msg := range stream {
		writeLine(encoder, flusher, canFlush, msg)
	}

	result := agent.Result()
	writeLine(encoder, flusher, canFlush, map[string]any{
		"type":    "result",
		"answer":  result.Answer,
		"stopped": result.Stopped,
		"error":   errorText(result.Err),
	})
}

func newAgent() *base.BaseAgent {
	agent := base.NewBaseAgent(
		"httpapi",
		"HTTP example agent",
		systemPrompt,
		envOr("LLM_MODEL", "gpt-4o"),
		os.Getenv("LLM_TOKEN"),
		os.Getenv("LLM_BASE_URL"),
		finishTool{},
	)
	agent.AddTool(countRowsTool{})
	return agent
}

func writeLine(encoder *json.Encoder, flusher http.Flusher, canFlush bool, payload any) {
	if err := encoder.Encode(payload); err != nil {
		return // the client is gone; the run context will be cancelled anyway
	}
	if canFlush {
		flusher.Flush()
	}
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprint(err)
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
