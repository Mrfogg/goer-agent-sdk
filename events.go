package base

import (
	"context"
	"strings"
	"time"

	"github.com/excelmatic/goer-agent-sdk/xlog"
)

// dispatchToolEvent delivers a run event to the consumer of Run. Delivery is
// cancelled together with the run context, so a consumer that stops reading the
// channel can never block the run goroutine forever.
func dispatchToolEvent(ctx context.Context, msgChan chan Msg, event Msg) {
	if !sendEvent(msgChan, event, ctx.Done(), 0) {
		xlog.Debug("dropped run event after run finished: type=%s", event.Type)
	}
}

// dispatchFinalEvent delivers a terminal event. It ignores the run context
// because the terminal event must survive cancellation, but it gives up after
// finalEventDeliveryTimeout so an abandoned channel cannot leak the goroutine.
func dispatchFinalEvent(msgChan chan Msg, event Msg) {
	if !sendEvent(msgChan, event, nil, finalEventDeliveryTimeout) {
		xlog.Warn("dropped terminal run event after %s: type=%s", finalEventDeliveryTimeout, event.Type)
	}
}

// sendEvent reports whether the event was handed to the channel. A nil done
// channel and a zero timeout mean "wait for as long as the consumer needs".
func sendEvent(msgChan chan Msg, event Msg, done <-chan struct{}, timeout time.Duration) bool {
	if msgChan == nil {
		return false
	}
	if strings.TrimSpace(event.Type) == "" {
		return false
	}
	defer func() {
		// The channel can be closed by a concurrent consumer; dropping the event is
		// safe, panicking is not.
		if recovered := recover(); recovered != nil {
			xlog.Warn("dropped run event after channel closed: type=%s", event.Type)
		}
	}()

	var timeoutCh <-chan time.Time
	if timeout > 0 {
		timeoutCh = time.After(timeout)
	}
	if done == nil && timeoutCh == nil {
		msgChan <- event
		return true
	}
	select {
	case msgChan <- event:
		return true
	case <-done:
		return false
	case <-timeoutCh:
		return false
	}
}

// startRunHeartbeat emits a heartbeat every heartbeatInterval until the returned
// stop function is called.
func (a *BaseAgent) startRunHeartbeat(ctx context.Context, msgChan chan Msg) func() {
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()

		send := func() bool {
			event := Msg{
				Type:    MsgTypeHeartbeat,
				Content: "running",
				Data: map[string]any{
					"timestamp": time.Now().Format(time.RFC3339Nano),
				},
			}
			select {
			case <-done:
				return false
			case <-ctx.Done():
				return false
			case msgChan <- event:
				return true
			default:
				// The consumer is behind: skip this beat instead of blocking.
				return true
			}
		}

		if !send() {
			return
		}
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if !send() {
					return
				}
			}
		}
	}()

	return func() {
		close(done)
		<-stopped
	}
}
