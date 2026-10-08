package interaction

import (
	"context"
	"fmt"
	"strings"
	"time"

	base "github.com/Mrfogg/goer-agent-sdk"
	"github.com/Mrfogg/goer-agent-sdk/ctxkey"

	"github.com/sashabaranov/go-openai"
	"github.com/sashabaranov/go-openai/jsonschema"
)

// Tool names as the model sees them.
const (
	AskUserToolName           = "ask_user"
	RequestPermissionToolName = "request_permission"
)

// Option values the permission tool offers and understands.
const (
	permissionAllow  = "allow"
	permissionDeny   = "deny"
	permissionAlways = "always"
)

// AskUserTool asks the user a question and suspends the run until they answer.
// Register it as a regular tool: the answer becomes the tool result and the run
// carries on.
//
// It is usable only when a Registry is on the run context. The host puts one
// there before calling Run; without it the tool fails with a tool result the
// model can read, rather than panicking.
type AskUserTool struct {
	// Timeout bounds how long the run waits for an answer. Zero waits until the
	// run's context is cancelled (Stop, disconnect, or the host's own deadline).
	Timeout time.Duration
}

func (AskUserTool) Name() string { return AskUserToolName }

func (AskUserTool) Description() string {
	return `Ask the user a question and wait for their answer.
Parameters:
- question (string, REQUIRED): the question to ask.
- options (array of string, OPTIONAL): suggested answers; omit for a free-text question.
Use it only when the answer cannot be inferred from the conversation or the tools. The run pauses until the user answers, and their answer becomes this tool's result.`
}

func (AskUserTool) OpenAIFunctionDefinition() *openai.FunctionDefinition {
	return &openai.FunctionDefinition{
		Name:        AskUserToolName,
		Description: AskUserTool{}.Description(),
		Parameters: base.OpenAIObjectSchema(map[string]jsonschema.Definition{
			"question": base.OpenAIStringSchema("The question to ask the user."),
			"options": base.OpenAIArraySchema(
				base.OpenAIStringSchema("A suggested answer."),
				"Suggested answers to offer; omit for a free-text question.",
			),
		}, "question"),
	}
}

func (t AskUserTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	question := strings.TrimSpace(stringArg(args["question"]))
	if question == "" {
		return base.ToolResult{Success: false, Error: "question is required"}, nil
	}

	resp, err := park(ctx, Request{
		Kind:      KindQuestion,
		Title:     question,
		Options:   optionList(args["options"]),
		AllowText: true,
	}, t.Timeout)
	if err != nil {
		return base.ToolResult{Success: false, Error: err.Error()}, nil
	}

	answer := strings.TrimSpace(resp.Text)
	if answer == "" {
		answer = strings.TrimSpace(resp.Choice)
	}
	if answer == "" {
		return base.ToolResult{Success: false, Error: "the user gave no answer"}, nil
	}

	modelData := map[string]any{"answer": answer}
	if resp.Choice != "" {
		modelData["choice"] = resp.Choice
	}
	return base.ToolResult{
		Success:      true,
		ModelContent: fmt.Sprintf("The user answered: %s", answer),
		ModelData:    modelData,
	}, nil
}

// RequestPermissionTool asks the user to approve an action and suspends the run
// until they decide. The decision — not the action — is the tool result: a
// denial is a successful call that returned "no", so the model can adapt instead
// of treating the run as broken.
type RequestPermissionTool struct {
	// Timeout bounds how long the run waits for a decision. Zero waits until the
	// run's context is cancelled.
	Timeout time.Duration
	// AllowAlways also offers a standing "always allow" choice. The tool only
	// reports it back in Scope; persisting the grant is the host's job.
	AllowAlways bool
}

func (RequestPermissionTool) Name() string { return RequestPermissionToolName }

func (RequestPermissionTool) Description() string {
	return `Ask the user to approve an action before you take it.
Parameters:
- action (string, REQUIRED): the concrete action you want to perform, in one line.
- reason (string, OPTIONAL): why the action is needed.
The run pauses until the user approves or denies. A denial is not an error: you get {approved:false} and should adapt or report the gap instead of retrying the same action.`
}

func (RequestPermissionTool) OpenAIFunctionDefinition() *openai.FunctionDefinition {
	return &openai.FunctionDefinition{
		Name:        RequestPermissionToolName,
		Description: RequestPermissionTool{}.Description(),
		Parameters: base.OpenAIObjectSchema(map[string]jsonschema.Definition{
			"action": base.OpenAIStringSchema("The concrete action to approve, in one line."),
			"reason": base.OpenAIStringSchema("Why the action is needed."),
		}, "action"),
	}
}

func (t RequestPermissionTool) Execute(ctx context.Context, args map[string]any) (base.ToolResult, error) {
	action := strings.TrimSpace(stringArg(args["action"]))
	if action == "" {
		return base.ToolResult{Success: false, Error: "action is required"}, nil
	}
	reason := strings.TrimSpace(stringArg(args["reason"]))

	options := []Option{
		{Value: permissionAllow, Label: "Allow"},
		{Value: permissionDeny, Label: "Deny"},
	}
	if t.AllowAlways {
		options = append(options, Option{Value: permissionAlways, Label: "Always allow"})
	}

	resp, err := park(ctx, Request{
		Kind:    KindPermission,
		Title:   action,
		Body:    reason,
		Options: options,
	}, t.Timeout)
	if err != nil {
		return base.ToolResult{Success: false, Error: err.Error()}, nil
	}

	approved := resp.Approved || resp.Choice == permissionAllow || resp.Choice == permissionAlways
	scope := resp.Scope
	if scope == "" {
		scope = ScopeOnce
	}
	if resp.Choice == permissionAlways {
		scope = ScopeAlways
	}

	verdict := "denied"
	if approved {
		verdict = "approved"
	}
	return base.ToolResult{
		Success:      true,
		ModelContent: fmt.Sprintf("The user %s the action: %s", verdict, action),
		ModelData:    map[string]any{"approved": approved, "scope": scope},
	}, nil
}

// park registers a request, announces it on the product channel, then blocks
// until it is answered. Registration happens before the announcement so a fast
// answer can never race it and be dropped as unknown.
func park(ctx context.Context, req Request, timeout time.Duration) (Response, error) {
	registry, ok := ctx.Value(ctxkey.InteractionRegistry).(*Registry)
	if !ok || registry == nil {
		return Response{}, fmt.Errorf("no interaction registry on the run context")
	}
	emit, ok := emitterFrom(ctx)
	if !ok || emit == nil {
		return Response{}, fmt.Errorf("no tool event emitter on the run context")
	}

	waiter := registry.Open(req)
	defer registry.Close(waiter.ID())

	emit(base.Msg{Type: MsgTypeRequest, Data: requestData(waiter.Request())})

	waitCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	resp, waitErr := waiter.Wait(waitCtx)
	if waitErr != nil {
		// Abandoned: nobody answered. Emit it on the parent context so the notice
		// still reaches a client when only the timeout (not the run) expired.
		emit(base.Msg{Type: MsgTypeResolved, Data: map[string]any{
			"id":      waiter.ID(),
			"outcome": "abandoned",
		}})
		return Response{}, fmt.Errorf("the user did not answer: %w", waitErr)
	}

	emit(base.Msg{Type: MsgTypeResolved, Data: resolvedData(resp)})
	return resp, nil
}

// emitterFrom reads the run's product channel. The runtime stores it as the
// named base.ToolEventEmitter; tests and hosts sometimes store a bare func(Msg).
// Both name the same underlying type, so accept either.
func emitterFrom(ctx context.Context) (func(base.Msg), bool) {
	switch emit := ctx.Value(ctxkey.ToolEventEmitter).(type) {
	case base.ToolEventEmitter:
		return emit, true
	case func(base.Msg):
		return emit, true
	default:
		return nil, false
	}
}

// requestData renders a Request as the payload a client needs to show the
// prompt and reply: an id, a kind, and whatever the user must see to decide.
func requestData(req Request) map[string]any {
	data := map[string]any{
		"id":   req.ID,
		"kind": string(req.Kind),
	}
	if req.Title != "" {
		data["title"] = req.Title
	}
	if req.Body != "" {
		data["body"] = req.Body
	}
	if len(req.Options) > 0 {
		options := make([]map[string]any, 0, len(req.Options))
		for _, option := range req.Options {
			options = append(options, map[string]any{"value": option.Value, "label": option.Label})
		}
		data["options"] = options
	}
	if req.AllowText {
		data["allow_text"] = true
	}
	if req.Multi {
		data["multi"] = true
	}
	if len(req.Meta) > 0 {
		data["meta"] = req.Meta
	}
	return data
}

// resolvedData renders a Response as the payload a client uses to clear the
// prompt.
func resolvedData(resp Response) map[string]any {
	data := map[string]any{"id": resp.ID, "outcome": "answered"}
	if resp.Approved {
		data["approved"] = true
	}
	if resp.Choice != "" {
		data["choice"] = resp.Choice
	}
	if resp.Text != "" {
		data["text"] = resp.Text
	}
	if resp.Scope != "" {
		data["scope"] = resp.Scope
	}
	return data
}

func stringArg(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}

// optionList normalizes the model's suggested answers, which arrive from JSON as
// either []any or []string, into Options whose value and label are the text.
func optionList(value any) []Option {
	switch typed := value.(type) {
	case nil:
		return nil
	case []string:
		options := make([]Option, 0, len(typed))
		for _, text := range typed {
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				options = append(options, Option{Value: trimmed, Label: trimmed})
			}
		}
		return options
	case []any:
		options := make([]Option, 0, len(typed))
		for _, item := range typed {
			if trimmed := strings.TrimSpace(stringArg(item)); trimmed != "" {
				options = append(options, Option{Value: trimmed, Label: trimmed})
			}
		}
		return options
	default:
		return nil
	}
}
