package interaction

import (
	"context"
	"errors"
	"testing"
	"time"
)

func waitContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 2*time.Second)
}

func TestResolveWakesWaiter(t *testing.T) {
	registry := NewRegistry()
	waiter := registry.Open(Request{Kind: KindQuestion, Title: "which sheet?"})

	done := make(chan error, 1)
	go func() {
		done <- registry.Resolve(Response{ID: waiter.ID(), Text: "Sheet1"})
	}()

	ctx, cancel := waitContext(t)
	defer cancel()
	resp, err := waiter.Wait(ctx)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if resp.Text != "Sheet1" {
		t.Fatalf("text = %q, want %q", resp.Text, "Sheet1")
	}
	if err := <-done; err != nil {
		t.Fatalf("resolve: %v", err)
	}
}

func TestResolveUnknownIDIsRefused(t *testing.T) {
	registry := NewRegistry()
	if err := registry.Resolve(Response{ID: "nope"}); !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("err = %v, want ErrUnknownRequest", err)
	}
}

func TestResolveTwiceIsRefused(t *testing.T) {
	registry := NewRegistry()
	waiter := registry.Open(Request{Kind: KindPermission})

	registry.Resolve(Response{ID: waiter.ID(), Approved: true})

	ctx, cancel := waitContext(t)
	defer cancel()
	if _, err := waiter.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}
	if err := registry.Resolve(Response{ID: waiter.ID(), Approved: false}); !errors.Is(err, ErrAlreadyResolved) {
		t.Fatalf("second resolve err = %v, want ErrAlreadyResolved", err)
	}
}

func TestCloseThenResolveIsUnknown(t *testing.T) {
	registry := NewRegistry()
	waiter := registry.Open(Request{Kind: KindQuestion})
	registry.Close(waiter.ID())

	if err := registry.Resolve(Response{ID: waiter.ID()}); !errors.Is(err, ErrUnknownRequest) {
		t.Fatalf("err = %v, want ErrUnknownRequest", err)
	}
}

func TestPendingListsUnresolvedRequests(t *testing.T) {
	registry := NewRegistry()
	question := registry.Open(Request{Kind: KindQuestion, Title: "a"})
	permission := registry.Open(Request{Kind: KindPermission, Title: "b"})

	if got := len(registry.Pending()); got != 2 {
		t.Fatalf("pending = %d, want 2", got)
	}

	registry.Resolve(Response{ID: question.ID(), Text: "x"})
	ctx, cancel := waitContext(t)
	defer cancel()
	if _, err := question.Wait(ctx); err != nil {
		t.Fatalf("wait: %v", err)
	}

	pending := registry.Pending()
	if len(pending) != 1 || pending[0].ID != permission.ID() {
		t.Fatalf("pending after one answer = %+v, want only %q", pending, permission.ID())
	}

	registry.Close(permission.ID())
	if got := len(registry.Pending()); got != 0 {
		t.Fatalf("pending after close = %d, want 0", got)
	}
}

func TestWaitReturnsWhenContextIsDone(t *testing.T) {
	registry := NewRegistry()
	waiter := registry.Open(Request{Kind: KindQuestion})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := waiter.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestOpenHandsOutUniqueIDs(t *testing.T) {
	registry := NewRegistry("s1-")
	seen := make(map[string]bool)
	for i := 0; i < 5; i++ {
		id := registry.Open(Request{}).ID()
		if id == "" {
			t.Fatal("empty id")
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestRequestCopyIsIndependent(t *testing.T) {
	registry := NewRegistry()
	waiter := registry.Open(Request{
		Kind:    KindPermission,
		Options: []Option{{Value: "allow", Label: "Allow"}},
		Meta:    map[string]any{"tool": "delete"},
	})

	copied := waiter.Request()
	copied.Options[0].Value = "mutated"
	copied.Meta["tool"] = "mutated"

	fresh := waiter.Request()
	if fresh.Options[0].Value != "allow" || fresh.Meta["tool"] != "delete" {
		t.Fatalf("Request() leaks internal state: %+v", fresh)
	}
}
