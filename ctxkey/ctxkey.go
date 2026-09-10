// Package ctxkey defines the context keys used to exchange per-run state
// between the agent runtime and Tool implementations.
package ctxkey

// ContextKey is the type of context keys understood by the SDK.
type ContextKey string

const (
	// ChatID carries the identifier of the current chat/session, if any.
	ChatID ContextKey = "chatID"

	// AgentHistory carries the agent's accumulated conversation history.
	// The stored value is a []github.com/sashabaranov/go-openai.ChatCompletionMessage.
	AgentHistory ContextKey = "agentHistory"

	// ToolEventEmitter carries a function tools can use to emit events back to
	// the agent runtime. The stored value is a func(Msg).
	ToolEventEmitter ContextKey = "toolEventEmitter"
)
