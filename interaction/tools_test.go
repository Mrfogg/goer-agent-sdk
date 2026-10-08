package interaction

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	base "github.com/Mrfogg/goer-agent-sdk"
	"github.com/Mrfogg/goer-agent-sdk/ctxkey"
)

// recordingEmitter stands in for the run's product channel. It records every
// message and signals on a channel so a test can wait for one to arrive.
type recordingEmitter struct {
	mu     sync.Mutex
	msgs   []base.Msg
	events chan base.Msg
}

func newRecordingEmitter() *recordingEmitter {
	return &recordingEmitter{events: make(chan base.Msg, 16)}
}

func (e *recordingEmitter) emit(msg base.Msg) {
	e.mu.Lock()
	e.msgs = append(e.msgs, msg)
	e.mu.Unlock()
	e.events <- msg
}

func (e *recordingEmitter) messages() []base.Msg {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]base.Msg(nil), e.msgs...)
}

// toolContext builds the context the runtime hands a tool: the run context with
// the host's registry and the runtime's event emitter attached.
func toolContext(registry *Registry, emitter *recordingEmitter) context.Context {
	ctx := context.WithValue(context.Background(), ctxkey.InteractionRegistry, registry)
	return context.WithValue(ctx, ctxkey.ToolEventEmitter, emitter.emit)
}

func firstEventOfType(t *testing.T, emitter *recordingEmitter, msgType string) base.Msg {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case msg := <-emitter.events:
			if msg.Type == msgType {
				return msg
			}
		case <-deadline:
			t.Fatalf("no %q event emitted", msgType)
		}
	}
}

func TestAskUserToolAnnouncesThenWaitsThenReturnsTheAnswer(t *testing.T) {
	registry := NewRegistry()
	emitter := newRecordingEmitter()
	ctx, cancel := context.WithTimeout(toolContext(registry, emitter), 5*time.Second)
	defer cancel()

	type outcome struct {
		result base.ToolResult
	}
	done := make(chan outcome, 1)
	go func() {
		result, _ := AskUserTool{}.Execute(ctx, map[string]any{"question": "Which sheet?"})
		done <- outcome{result: result}
	}()

	// The request must be announced before the tool blocks.
	event := firstEventOfType(t, emitter, MsgTypeRequest)
	id, _ := event.Data["id"].(string)
	if id == "" {
		t.Fatal("the request event carries no id")
	}
	if kind, _ := event.Data["kind"].(string); kind != string(KindQuestion) {
		t.Fatalf("kind = %q, want %q", kind, KindQuestion)
	}
	if title, _ := event.Data["title"].(string); title != "Which sheet?" {
		t.Fatalf("title = %q", title)
	}

	// With no answer yet the tool is still parked.
	select {
	case got := <-done:
		t.Fatalf("the tool returned before an answer: %+v", got.result)
	case <-time.After(50 * time.Millisecond):
	}

	if err := registry.Resolve(Response{ID: id, Text: "Sheet1"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	select {
	case got := <-done:
		if !got.result.Success {
			t.Fatalf("result = %+v", got.result)
		}
		if answer, _ := got.result.ModelData["answer"].(string); answer != "Sheet1" {
			t.Fatalf("answer = %q, want %q", answer, "Sheet1")
		}
		if !strings.Contains(got.result.ModelContent, "Sheet1") {
			t.Fatalf("model content = %q", got.result.ModelContent)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the tool did not return after being answered")
	}
}

func TestAskUserToolOffersChoices(t *testing.T) {
	registry := NewRegistry()
	emitter := newRecordingEmitter()
	ctx, cancel := context.WithTimeout(toolContext(registry, emitter), 5*time.Second)
	defer cancel()

	done := make(chan base.ToolResult, 1)
	go func() {
		result, _ := AskUserTool{}.Execute(ctx, map[string]any{
			"question": "Which one?",
			"options":  []any{"first", "second"},
		})
		done <- result
	}()

	event := firstEventOfType(t, emitter, MsgTypeRequest)
	options, _ := event.Data["options"].([]map[string]any)
	if len(options) != 2 || options[0]["value"] != "first" {
		t.Fatalf("options = %+v", event.Data["options"])
	}

	id, _ := event.Data["id"].(string)
	if err := registry.Resolve(Response{ID: id, Choice: "second"}); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	result := <-done
	if answer, _ := result.ModelData["answer"].(string); answer != "second" {
		t.Fatalf("answer = %q, want %q", answer, "second")
	}
}

func TestRequestPermissionToolReportsTheDecision(t *testing.T) {
	cases := []struct {
		name         string
		response     Response
		wantApproved bool
		wantScope    string
	}{
		{"allow-by-choice", Response{Choice: permissionAllow}, true, ScopeOnce},
		{"allow-by-flag", Response{Approved: true}, true, ScopeOnce},
		{"deny", Response{Choice: permissionDeny}, false, ScopeOnce},
		{"always", Response{Choice: permissionAlways, Scope: ScopeAlways}, true, ScopeAlways},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			registry := NewRegistry()
			emitter := newRecordingEmitter()
			ctx, cancel := context.WithTimeout(toolContext(registry, emitter), 5*time.Second)
			defer cancel()

			done := make(chan base.ToolResult, 1)
			go func() {
				result, _ := RequestPermissionTool{AllowAlways: true}.Execute(ctx, map[string]any{
					"action": "delete every row",
					"reason": "cleanup",
				})
				done <- result
			}()

			event := firstEventOfType(t, emitter, MsgTypeRequest)
			if kind, _ := event.Data["kind"].(string); kind != string(KindPermission) {
				t.Fatalf("kind = %q, want %q", kind, KindPermission)
			}

			id, _ := event.Data["id"].(string)
			if err := registry.Resolve(Response{
				ID:       id,
				Approved: tc.response.Approved,
				Choice:   tc.response.Choice,
				Scope:    tc.response.Scope,
			}); err != nil {
				t.Fatalf("resolve: %v", err)
			}

			result := <-done
			if !result.Success {
				t.Fatalf("a decision must be a successful call: %+v", result)
			}
			if approved, _ := result.ModelData["approved"].(bool); approved != tc.wantApproved {
				t.Fatalf("approved = %v, want %v", approved, tc.wantApproved)
			}
			if scope, _ := result.ModelData["scope"].(string); scope != tc.wantScope {
				t.Fatalf("scope = %q, want %q", scope, tc.wantScope)
			}
		})
	}
}

func TestToolsFailWithoutARegistry(t *testing.T) {
	ctx := context.Background()

	permission, err := RequestPermissionTool{}.Execute(ctx, map[string]any{"action": "x"})
	if err != nil {
		t.Fatalf("execute err: %v", err)
	}
	if permission.Success || !strings.Contains(permission.Error, "registry") {
		t.Fatalf("result = %+v, want a failure naming the missing registry", permission)
	}

	question, err := AskUserTool{}.Execute(ctx, map[string]any{"question": "x"})
	if err != nil {
		t.Fatalf("execute err: %v", err)
	}
	if question.Success || !strings.Contains(question.Error, "registry") {
		t.Fatalf("result = %+v, want a failure naming the missing registry", question)
	}
}

func TestToolsRequireTheirArguments(t *testing.T) {
	registry := NewRegistry()
	emitter := newRecordingEmitter()
	ctx := toolContext(registry, emitter)

	question, _ := AskUserTool{}.Execute(ctx, map[string]any{"question": "   "})
	if question.Success || question.Error != "question is required" {
		t.Fatalf("result = %+v", question)
	}

	permission, _ := RequestPermissionTool{}.Execute(ctx, map[string]any{})
	if permission.Success || permission.Error != "action is required" {
		t.Fatalf("result = %+v", permission)
	}
}

func TestToolAbandonsOnTimeout(t *testing.T) {
	registry := NewRegistry()
	emitter := newRecordingEmitter()
	ctx := toolContext(registry, emitter)

	result, err := AskUserTool{Timeout: 30 * time.Millisecond}.Execute(ctx, map[string]any{"question": "anyone there?"})
	if err != nil {
		t.Fatalf("execute err: %v", err)
	}
	if result.Success {
		t.Fatalf("result = %+v, want a failure when nobody answers", result)
	}
	if len(registry.Pending()) != 0 {
		t.Fatal("an abandoned request must be closed, not left pending")
	}

	resolved := firstEventOfType(t, emitter, MsgTypeResolved)
	if outcome, _ := resolved.Data["outcome"].(string); outcome != "abandoned" {
		t.Fatalf("resolved outcome = %q, want %q", outcome, "abandoned")
	}
}

func TestToolAbandonsWhenTheRunIsCancelled(t *testing.T) {
	registry := NewRegistry()
	emitter := newRecordingEmitter()
	ctx, cancel := context.WithCancel(toolContext(registry, emitter))
	defer cancel()

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	result, err := RequestPermissionTool{}.Execute(ctx, map[string]any{"action": "delete rows"})
	if err != nil {
		t.Fatalf("execute err: %v", err)
	}
	if result.Success {
		t.Fatalf("result = %+v, want a failure when the run is cancelled", result)
	}
	if len(registry.Pending()) != 0 {
		t.Fatal("a cancelled request must be closed, not left pending")
	}
}
