package interaction

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
)

// ErrUnknownRequest is returned when an answer names a request that is not
// parked in the registry: it was never opened, or it was already closed.
var ErrUnknownRequest = errors.New("interaction: unknown request id")

// ErrAlreadyResolved is returned when a parked request is answered twice. The
// first answer wins; the second is refused so a duplicate frame cannot change an
// outcome that was already delivered.
var ErrAlreadyResolved = errors.New("interaction: request already resolved")

// Waiter is a parked request the tool blocks on until it is answered.
type Waiter struct {
	request  Request
	answers  chan Response
	resolved bool
}

// ID returns the request id clients echo back when they answer.
func (w *Waiter) ID() string { return w.request.ID }

// Request returns a copy of the parked request, for the transport to render.
func (w *Waiter) Request() Request { return cloneRequest(w.request) }

// Wait blocks until the request is answered or ctx is done. A done context means
// the answer never came: the run was stopped, the connection dropped, or a
// timeout fired. The caller decides what the tool should report back.
func (w *Waiter) Wait(ctx context.Context) (Response, error) {
	select {
	case resp := <-w.answers:
		return resp, nil
	case <-ctx.Done():
		return Response{}, ctx.Err()
	}
}

// Registry holds the requests a run has parked and is still waiting on. It is
// safe for concurrent use: the run parks and blocks on one goroutine while the
// transport answers from another.
type Registry struct {
	mu      sync.Mutex
	waiters map[string]*Waiter
	seq     atomic.Uint64
	prefix  string
}

// NewRegistry creates an empty registry. An optional prefix namespaces the ids
// it hands out; ids are unique within one registry.
func NewRegistry(prefix ...string) *Registry {
	name := ""
	if len(prefix) > 0 {
		name = prefix[0]
	}
	return &Registry{waiters: make(map[string]*Waiter), prefix: name}
}

// Open parks a request and returns the waiter to block on.
//
// Register before announcing: a client cannot answer a request it has not seen,
// but opening first means a fast answer can never race the registration and be
// dropped as unknown.
func (r *Registry) Open(req Request) *Waiter {
	w := &Waiter{request: req, answers: make(chan Response, 1)}
	w.request.ID = fmt.Sprintf("%s%d", r.prefix, r.seq.Add(1))

	r.mu.Lock()
	r.waiters[w.request.ID] = w
	r.mu.Unlock()
	return w
}

// Resolve delivers an answer to whoever is waiting on it. The first answer wins:
// a second returns ErrAlreadyResolved, and an unknown id returns
// ErrUnknownRequest. Delivery never blocks, so the transport can call this from
// its read loop without waiting for the run to be scheduled.
func (r *Registry) Resolve(resp Response) error {
	r.mu.Lock()
	w, ok := r.waiters[resp.ID]
	switch {
	case !ok:
		r.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrUnknownRequest, resp.ID)
	case w.resolved:
		r.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrAlreadyResolved, resp.ID)
	}
	w.resolved = true
	r.mu.Unlock()

	w.answers <- resp
	return nil
}

// Close drops a request once its tool is done with it, whether it was answered
// or abandoned. A late Resolve then reports ErrUnknownRequest instead of
// changing an outcome that no longer exists.
func (r *Registry) Close(id string) {
	r.mu.Lock()
	delete(r.waiters, id)
	r.mu.Unlock()
}

// Pending returns the requests still waiting for an answer. The transport calls
// it on reconnect to re-render whatever the client may have missed.
func (r *Registry) Pending() []Request {
	r.mu.Lock()
	defer r.mu.Unlock()

	pending := make([]Request, 0, len(r.waiters))
	for _, w := range r.waiters {
		if w.resolved {
			continue
		}
		pending = append(pending, cloneRequest(w.request))
	}
	return pending
}

// cloneRequest copies the request, including its nested slices and map, so the
// registry and its callers never share mutable state.
func cloneRequest(req Request) Request {
	if len(req.Options) > 0 {
		req.Options = append([]Option(nil), req.Options...)
	}
	if len(req.Meta) > 0 {
		meta := make(map[string]any, len(req.Meta))
		for key, value := range req.Meta {
			meta[key] = value
		}
		req.Meta = meta
	}
	return req
}
