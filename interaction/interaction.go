// Package interaction implements agent-initiated requests to the user — a
// question or a permission prompt — as ordinary tool calls that suspend until
// they are answered.
//
// The runtime is untouched. A tool parks a request in a Registry, announces it
// on the product channel, and blocks inside Execute. The host answers with
// Registry.Resolve, and the tool returns the answer as its ToolResult, which the
// engine records as the tool call's result — so the model reads "I called
// request_permission and the result was {approved:true}" and carries on.
//
// Wiring is three lines on the host side:
//
//	reg := interaction.NewRegistry(sessionID)
//	ctx = context.WithValue(ctx, ctxkey.InteractionRegistry, reg)
//	stream, _ := agent.Run(ctx, input) // the tools now suspend on reg
//
// and, when a client answer arrives:
//
//	reg.Resolve(interaction.Response{ID: id, Choice: "allow"})
//
// This package owns only the request/response shapes, the waiting registry and
// the tools. Transport, routing policy and persistence belong to the host.
package interaction

// Kind tells the client what sort of answer a request expects.
type Kind string

const (
	// KindQuestion asks the user for an answer, free text and/or a choice.
	KindQuestion Kind = "question"
	// KindPermission asks the user to approve or deny an action.
	KindPermission Kind = "permission"
)

// The stream messages the tools emit on the product channel, through the tool
// event emitter. The model never sees them: they exist so a client can render
// the pending request and clear it once it is answered.
const (
	// MsgTypeRequest announces a newly parked request. Data carries the Request
	// fields, including the id the client must echo back in its answer.
	MsgTypeRequest = "interaction_request"
	// MsgTypeResolved announces that a request was answered or abandoned. Data
	// carries id and outcome.
	MsgTypeResolved = "interaction_resolved"
)

// Scope values a permission response may report. Scope is informational: the
// tool only passes it to the model, and persisting a standing grant is the
// host's job.
const (
	ScopeOnce   = "once"
	ScopeAlways = "always"
)

// Option is one selectable answer offered alongside a request.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Request is one parked question or permission prompt.
type Request struct {
	// ID is assigned by the registry when the request is opened. Clients echo it
	// back so the answer can be routed to the waiter.
	ID string `json:"id"`
	// Kind is KindQuestion or KindPermission.
	Kind Kind `json:"kind"`
	// Title is the question, or the action being requested.
	Title string `json:"title,omitempty"`
	// Body is supporting text: why the action is needed, extra context.
	Body string `json:"body,omitempty"`
	// Options are the selectable answers; empty means free text only.
	Options []Option `json:"options,omitempty"`
	// AllowText admits a typed answer even when Options are offered.
	AllowText bool `json:"allow_text,omitempty"`
	// Multi admits more than one selected option.
	Multi bool `json:"multi,omitempty"`
	// Meta carries host bookkeeping. The runtime never interprets it.
	Meta map[string]any `json:"meta,omitempty"`
}

// Response is the user's answer to a Request.
type Response struct {
	// ID is the request being answered.
	ID string `json:"id"`
	// Approved is the decision for a permission request.
	Approved bool `json:"approved,omitempty"`
	// Choice is the selected Option.Value, when the user picked one.
	Choice string `json:"choice,omitempty"`
	// Text is a free-text answer.
	Text string `json:"text,omitempty"`
	// Scope is ScopeOnce or ScopeAlways for a permission request.
	Scope string `json:"scope,omitempty"`
}
