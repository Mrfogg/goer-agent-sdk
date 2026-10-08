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

	// InteractionRegistry carries the registry that agent-initiated questions and
	// permission requests are parked in while the run waits for an answer. The
	// stored value is a *github.com/Mrfogg/goer-agent-sdk/interaction.Registry.
	//
	// The runtime does not set this key: the host places it on the context it
	// passes to Run, and the interaction tools read it back. Keeping it
	// host-owned is what lets the tools suspend without the engine knowing.
	InteractionRegistry ContextKey = "interactionRegistry"
)
