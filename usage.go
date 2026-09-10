package base

import "github.com/sashabaranov/go-openai"

// TokenUsage is the token accounting of one LLM call.
type TokenUsage struct {
	// PromptTokens is the input (prompt) token count.
	PromptTokens int `json:"prompt_tokens"`
	// CompletionTokens is the output token count.
	CompletionTokens int `json:"completion_tokens"`
	// TotalTokens is what the provider reported as the sum.
	TotalTokens int `json:"total_tokens"`
	// Model is the model that served the call.
	Model string `json:"model,omitempty"`
	// CachedTokens and ReasoningTokens are optional breakdowns: providers report
	// them only for some models.
	CachedTokens    int `json:"cached_tokens,omitempty"`
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// plus returns the sum of two usages, used to aggregate a run.
func (u TokenUsage) plus(other TokenUsage) TokenUsage {
	return TokenUsage{
		PromptTokens:     u.PromptTokens + other.PromptTokens,
		CompletionTokens: u.CompletionTokens + other.CompletionTokens,
		TotalTokens:      u.TotalTokens + other.TotalTokens,
		Model:            other.Model,
		CachedTokens:     u.CachedTokens + other.CachedTokens,
		ReasoningTokens:  u.ReasoningTokens + other.ReasoningTokens,
	}
}

// tokenUsageFromOpenAI converts the provider's usage object.
func tokenUsageFromOpenAI(usage openai.Usage, model string) TokenUsage {
	tokens := TokenUsage{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
		Model:            model,
	}
	if usage.PromptTokensDetails != nil {
		tokens.CachedTokens = usage.PromptTokensDetails.CachedTokens
	}
	if usage.CompletionTokensDetails != nil {
		tokens.ReasoningTokens = usage.CompletionTokensDetails.ReasoningTokens
	}
	return tokens
}

// usageMessage reports one LLM call's token usage on the run stream.
func usageMessage(usage TokenUsage) Msg {
	data := map[string]any{
		"prompt_tokens":     usage.PromptTokens,
		"completion_tokens": usage.CompletionTokens,
		"total_tokens":      usage.TotalTokens,
	}
	if usage.Model != "" {
		data["model"] = usage.Model
	}
	if usage.CachedTokens > 0 {
		data["cached_tokens"] = usage.CachedTokens
	}
	if usage.ReasoningTokens > 0 {
		data["reasoning_tokens"] = usage.ReasoningTokens
	}
	return Msg{Type: MsgTypeUsage, Data: data}
}
