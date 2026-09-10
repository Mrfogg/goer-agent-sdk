package base

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/sashabaranov/go-openai"
)

// rebuildLLMClient (re)builds the OpenAI-compatible client from the configured
// auth token, base URL, HTTP client and extra headers.
func (a *BaseAgent) rebuildLLMClient() {
	config := openai.DefaultConfig(a.authToken)
	if trimmed := strings.TrimSpace(a.baseURL); trimmed != "" {
		config.BaseURL = trimmed
	}
	config.HTTPClient = a.httpClientWithHeaders()
	a.client = openai.NewClientWithConfig(config)
}

// httpClientWithHeaders returns the configured HTTP client, wrapped so the extra
// headers are attached to every request. The caller's client is copied, never
// mutated.
func (a *BaseAgent) httpClientWithHeaders() *http.Client {
	client := a.httpClient
	if client == nil {
		client = &http.Client{}
	}
	if len(a.httpHeaders) == 0 {
		return client
	}

	wrapped := *client
	wrapped.Transport = &headerRoundTripper{base: client.Transport, headers: a.httpHeaders}
	return &wrapped
}

type headerRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (rt *headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	cloned.Header = req.Header.Clone()
	for key, value := range rt.headers {
		cloned.Header.Set(key, value)
	}

	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(cloned)
}

type llmResponse struct {
	Message      openai.ChatCompletionMessage
	FinishReason openai.FinishReason
}

// callLLMWithRetry performs one logical LLM call, retrying transport failures
// with bounded exponential backoff. Cancellation stops retrying immediately and
// the last failure is preserved in the returned error.
func (a *BaseAgent) callLLMWithRetry(ctx context.Context, messages []openai.ChatCompletionMessage, model, toolChoice string, onContent, onReasoning func(string)) (llmResponse, error) {
	delay := llmRetryBaseDelay
	var lastErr error

	for attempt := 1; attempt <= llmMaxAttempts; attempt++ {
		response, err := a.callLLM(ctx, messages, model, toolChoice, onContent, onReasoning)
		if err == nil {
			return response, nil
		}
		lastErr = err
		if ctxErr := ctx.Err(); ctxErr != nil {
			return llmResponse{}, ctxErr
		}
		if attempt == llmMaxAttempts {
			break
		}

		agentLogWarn(ctx, "llm call failed (attempt %d/%d), retrying in %s: %v", attempt, llmMaxAttempts, delay, err)
		select {
		case <-ctx.Done():
			return llmResponse{}, ctx.Err()
		case <-time.After(delay):
		}
		if delay *= 2; delay > llmRetryMaxDelay {
			delay = llmRetryMaxDelay
		}
	}

	return llmResponse{}, fmt.Errorf("llm call failed after %d attempts (model=%s): %w", llmMaxAttempts, model, lastErr)
}

// callLLM calls the large language model with streaming enabled and rebuilds the
// final assistant message from streamed deltas.
func (a *BaseAgent) callLLM(ctx context.Context, messages []openai.ChatCompletionMessage, model, toolChoice string, onContent, onReasoning func(string)) (llmResponse, error) {
	if strings.TrimSpace(model) == "" {
		model = a.model
	}
	if strings.TrimSpace(model) == "" {
		return llmResponse{}, errors.New("model is not configured: pass a model to NewBaseAgent or WithModel")
	}
	client := a.client
	if client == nil {
		return llmResponse{}, errors.New("openai client is not configured: pass a token and base URL to NewBaseAgent")
	}

	request := openai.ChatCompletionRequest{
		Model:    model,
		Messages: sanitizeMessagesForLLM(messages),
	}
	if tools := a.openAITools(); len(tools) > 0 {
		request.Tools = tools
		request.ToolChoice = toolChoice
	}
	if effort := a.reasoningEffort; effort != "" {
		request.ReasoningEffort = effort
		request.ChatTemplateKwargs = map[string]any{
			"enable_reasoning": true,
		}
		request.ExtraBody = map[string]any{
			"thinking": map[string]any{
				"enable": true,
			},
			"reasoning": map[string]any{
				"effort": effort,
			},
		}
	}

	stream, err := client.CreateChatCompletionStream(ctx, request)
	if err != nil {
		return llmResponse{}, fmt.Errorf("failed to call LLM (model=%s): %w", model, err)
	}
	defer stream.Close()

	response := llmResponse{
		Message: openai.ChatCompletionMessage{
			Role: openai.ChatMessageRoleAssistant,
		},
	}
	receivedChoice := false
	for {
		chunk, recvErr := stream.Recv()
		if recvErr != nil {
			if errors.Is(recvErr, io.EOF) {
				break
			}
			return llmResponse{}, fmt.Errorf("failed to read LLM stream (model=%s): %w", model, recvErr)
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		receivedChoice = true
		choice := chunk.Choices[0]
		if choice.Delta.Role != "" {
			response.Message.Role = choice.Delta.Role
		}
		if choice.Delta.Content != "" {
			response.Message.Content += choice.Delta.Content
			if onContent != nil && strings.TrimSpace(response.Message.Content) != "" {
				onContent(response.Message.Content)
			}
		}
		if choice.Delta.Refusal != "" {
			response.Message.Refusal += choice.Delta.Refusal
		}
		if choice.Delta.ReasoningContent != "" {
			response.Message.ReasoningContent += choice.Delta.ReasoningContent
			if onReasoning != nil && strings.TrimSpace(response.Message.ReasoningContent) != "" {
				onReasoning(response.Message.ReasoningContent)
			}
		}
		if choice.Delta.FunctionCall != nil {
			if response.Message.FunctionCall == nil {
				response.Message.FunctionCall = &openai.FunctionCall{}
			}
			response.Message.FunctionCall.Name += choice.Delta.FunctionCall.Name
			response.Message.FunctionCall.Arguments += choice.Delta.FunctionCall.Arguments
		}
		mergeToolCallDeltas(&response.Message.ToolCalls, choice.Delta.ToolCalls)
		if choice.FinishReason != "" && choice.FinishReason != openai.FinishReasonNull {
			response.FinishReason = choice.FinishReason
		}
	}
	if !receivedChoice {
		return llmResponse{}, fmt.Errorf("failed to call LLM (model=%s): empty choices", model)
	}

	return response, nil
}

func mergeToolCallDeltas(dst *[]openai.ToolCall, deltas []openai.ToolCall) {
	if len(deltas) == 0 {
		return
	}
	for i := range deltas {
		delta := deltas[i]
		toolIndex := 0
		if delta.Index != nil && *delta.Index > 0 {
			toolIndex = *delta.Index
		}
		for len(*dst) <= toolIndex {
			*dst = append(*dst, openai.ToolCall{})
		}
		tool := &(*dst)[toolIndex]
		if delta.ID != "" {
			tool.ID = delta.ID
		}
		if delta.Type != "" {
			tool.Type = delta.Type
		}
		tool.Function.Name += delta.Function.Name
		tool.Function.Arguments += delta.Function.Arguments
	}
}

// sanitizeMessagesForLLM normalizes the transcript for providers that reject
// empty content and message names.
func sanitizeMessagesForLLM(messages []openai.ChatCompletionMessage) []openai.ChatCompletionMessage {
	sanitized := make([]openai.ChatCompletionMessage, len(messages))
	copy(sanitized, messages)
	for i := range sanitized {
		sanitized[i].Name = ""
		if sanitized[i].Content == "" && len(sanitized[i].MultiContent) == 0 {
			sanitized[i].Content = " "
		}
		if sanitized[i].ReasoningContent == "" {
			sanitized[i].ReasoningContent = " "
		}
	}
	return sanitized
}

func (a *BaseAgent) openAITools() []openai.Tool {
	tools := a.GetTools()
	if len(tools) == 0 {
		return nil
	}

	toolNames := make([]string, 0, len(tools))
	for name := range tools {
		toolNames = append(toolNames, name)
	}
	sort.Strings(toolNames)

	definitions := make([]openai.Tool, 0, len(toolNames))
	for _, toolName := range toolNames {
		definitions = append(definitions, OpenAIToolDefinition(tools[toolName]))
	}
	return definitions
}
