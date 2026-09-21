package memorykit

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Mrfogg/goer-agent-sdk/ctxkey"

	"github.com/sashabaranov/go-openai"
)

const (
	// userQuoteScanLimit bounds how far back the check looks. Delete permission
	// comes from the user asking now, not from something said long ago.
	userQuoteScanLimit = 20
	// userQuoteMinRunes is the shortest quote that can serve as evidence.
	userQuoteMinRunes = 2
)

// VerifyUserQuote checks that the user really asked for this. The quote must
// appear verbatim (ignoring case, spacing, and punctuation) inside one of the
// conversation's recent user messages.
//
// This is the only gate on deletion, and it is deliberately not a parameter the
// model can skip: the quote is required, and the server checks it.
func VerifyUserQuote(ctx context.Context, quote string) error {
	normalized := normalizeQuote(quote)
	if utf8.RuneCountInString(normalized) < userQuoteMinRunes {
		return fmt.Errorf(
			"user_quote is required and must quote the user's own words; %q is too short to be evidence",
			strings.TrimSpace(quote),
		)
	}

	for _, message := range recentUserMessages(ctx) {
		if strings.Contains(normalizeQuote(message), normalized) {
			return nil
		}
	}

	return fmt.Errorf(
		"user_quote %q does not appear in any of the last %d user messages, so the user did not ask for this here. Only call memory_forget when the user's own words ask for the memory to go away or give it up, such as a request to forget it or an explicit instruction that makes it obsolete; quote those words verbatim. A memory you merely believe is stale or wrong is corrected by rewriting it with memory_write instead",
		strings.TrimSpace(quote), userQuoteScanLimit,
	)
}

// recentUserMessages returns the conversation's user messages, newest first.
// It reads the history the agent hands to every tool call, so it sees exactly
// what the model saw.
func recentUserMessages(ctx context.Context) []string {
	history, _ := ctx.Value(ctxkey.AgentHistory).([]openai.ChatCompletionMessage)
	if len(history) == 0 {
		return nil
	}

	messages := make([]string, 0, userQuoteScanLimit)
	for i := len(history) - 1; i >= 0 && len(messages) < userQuoteScanLimit; i-- {
		if history[i].Role != openai.ChatMessageRoleUser {
			continue
		}
		if content := strings.TrimSpace(history[i].Content); content != "" {
			messages = append(messages, content)
		}
	}
	return messages
}

// normalizeQuote keeps letters and digits only, so a quote still matches when
// the model changes spacing, punctuation, or letter case.
func normalizeQuote(text string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(text) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}
