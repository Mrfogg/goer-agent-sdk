package memorykit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Mrfogg/goer-agent-sdk/xlog"

	"github.com/sashabaranov/go-openai"
)

// mergeMinSources is the fewest records a merge can fold.
const mergeMinSources = 2

// Merged is the result of folding several memories: the merged text, plus the
// distinct facts the merger found in the sources. Carrying the fact list back
// is what makes "did it drop anything" checkable at all.
type Merged struct {
	Facts  []string
	Merged string
}

// Merger folds the contents of several memories into one.
//
// The SDK ships no LLM client of its own, so the host supplies the call: pass
// NewOpenAIMerger to use the agent's own provider, or implement this interface
// to merge with a different model, a different key, or something else entirely.
type Merger interface {
	Merge(ctx context.Context, contents []string) (Merged, error)
}

// MergerFunc adapts a plain function to Merger.
type MergerFunc func(ctx context.Context, contents []string) (Merged, error)

func (f MergerFunc) Merge(ctx context.Context, contents []string) (Merged, error) {
	if f == nil {
		return Merged{}, errors.New("merger is not configured")
	}
	return f(ctx, contents)
}

// mergeSystemPrompt is the entire job description of the merge step. It is the
// only thing the merge call receives besides the source records: no memory
// tools, no conversation context, and none of the routing policy, because a
// pure text transform has nothing to route.
const mergeSystemPrompt = `You fold several memory records into one record.

Rules:
- Preserve every distinct fact from the input records. Dropping a fact is a failure.
- Never add a fact that is not in the input records. Do not guess, generalize, or fill gaps.
- The inputs may be near duplicates of a single fact, or several related facts that belong together. Either way the merged record keeps every fact and only removes the repetition.
- Keep the user's own wording. Do not restyle, translate, or normalize terms.
- Write the merged record in the language of the input records.
- Keep it short: one or two sentences, and never longer than the input records combined.

Reply with JSON only, in this shape:
{"facts": ["<one entry per distinct fact found in the input>"], "merged": "<the merged record>"}`

// mergeOutput is the merger's structured product.
type mergeOutput struct {
	Facts  []string `json:"facts"`
	Merged string   `json:"merged"`
}

// openAIMerger is the ready-made Merger: one chat completion per merge, asking
// for JSON so the fact list comes back alongside the text.
type openAIMerger struct {
	client *openai.Client
	model  string
}

// NewOpenAIMerger returns a Merger that folds memories with one call on the
// given client and model.
func NewOpenAIMerger(client *openai.Client, model string) Merger {
	return &openAIMerger{client: client, model: model}
}

func (m *openAIMerger) Merge(ctx context.Context, contents []string) (Merged, error) {
	if m == nil || m.client == nil {
		return Merged{}, errors.New("merge client is not initialized")
	}
	if strings.TrimSpace(m.model) == "" {
		return Merged{}, errors.New("merge model is not configured")
	}
	if len(contents) < mergeMinSources {
		return Merged{}, fmt.Errorf("merging needs at least %d memories", mergeMinSources)
	}

	payload, err := json.Marshal(contents)
	if err != nil {
		return Merged{}, err
	}

	response, err := m.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: m.model,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: mergeSystemPrompt},
			{Role: openai.ChatMessageRoleUser, Content: string(payload)},
		},
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONObject,
		},
		Temperature: 0.2,
	})
	if err != nil {
		return Merged{}, fmt.Errorf("merge model call failed: %w", err)
	}
	if len(response.Choices) == 0 {
		return Merged{}, errors.New("merge model returned no choices")
	}

	var output mergeOutput
	if err := json.Unmarshal([]byte(extractJSONObject(response.Choices[0].Message.Content)), &output); err != nil {
		return Merged{}, fmt.Errorf("merge model returned unparsable output: %w", err)
	}
	return Merged{Facts: output.Facts, Merged: output.Merged}, nil
}

// validateMergeOutput enforces the merge contract on the merger's product, so
// a merge that invents detail or drops the text never reaches the store.
func validateMergeOutput(sources []Memory, merged Merged) (string, error) {
	text := strings.TrimSpace(merged.Merged)
	if text == "" {
		return "", errors.New("merge returned empty content")
	}
	if len(merged.Facts) == 0 {
		return "", errors.New("merge returned no fact list")
	}

	limit := 0
	for _, source := range sources {
		limit += utf8.RuneCountInString(source.Content)
	}
	if length := utf8.RuneCountInString(text); length > limit {
		return "", fmt.Errorf("merged content is %d characters, longer than the %d source characters combined", length, limit)
	}

	// A short fact list is the visible symptom of dropped detail. It is logged
	// rather than rejected: proving coverage is not something this layer can do.
	if len(merged.Facts) < len(sources) {
		xlog.Warn("memory merge produced fewer facts than sources: facts=%d sources=%d",
			len(merged.Facts), len(sources))
	}

	return text, nil
}

// extractJSONObject tolerates a model that wraps its JSON in prose or a code
// fence despite being asked for JSON only.
func extractJSONObject(raw string) string {
	trimmed := strings.TrimSpace(raw)
	start := strings.Index(trimmed, "{")
	end := strings.LastIndex(trimmed, "}")
	if start >= 0 && end > start {
		return trimmed[start : end+1]
	}
	return trimmed
}
