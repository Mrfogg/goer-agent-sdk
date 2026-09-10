package base

import (
	"context"
	"strings"

	"github.com/Mrfogg/goer-agent-sdk/xlog"
)

// dispatchToolEvent delivers one stream message to the consumer of Run.
//
// Delivery is cancelled together with the run context: a consumer that stops
// reading can stall the run only until it cancels the context (or calls Stop),
// never forever. Messages emitted once the run is over are dropped.
func dispatchToolEvent(ctx context.Context, msgChan chan Msg, event Msg) {
	if msgChan == nil {
		return
	}
	if strings.TrimSpace(event.Type) == "" {
		return
	}

	defer func() {
		// The channel can be closed by a concurrent consumer; dropping the message
		// is safe, panicking is not.
		if recovered := recover(); recovered != nil {
			xlog.Warn("dropped run message after channel closed: type=%s", event.Type)
		}
	}()

	select {
	case msgChan <- event:
	case <-ctx.Done():
		xlog.Debug("dropped run message after run finished: type=%s", event.Type)
	}
}
