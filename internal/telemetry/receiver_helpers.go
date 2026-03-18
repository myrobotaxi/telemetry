package telemetry

import (
	"context"
	"errors"
	"time"

	"github.com/coder/websocket"
)

// vehicleConn tracks the state of a single vehicle WebSocket connection.
type vehicleConn struct {
	vin       string
	conn      *websocket.Conn
	connected time.Time
	cancel    context.CancelFunc
}

// isNormalClose reports whether the error represents a normal WebSocket
// closure (client disconnecting cleanly or context cancelled).
func isNormalClose(err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	var closeErr websocket.CloseError
	if errors.As(err, &closeErr) {
		return closeErr.Code == websocket.StatusNormalClosure ||
			closeErr.Code == websocket.StatusGoingAway
	}
	return false
}

// deadlineFromCtx extracts the deadline from ctx, defaulting to 10 seconds
// from now if none is set.
func deadlineFromCtx(ctx context.Context) time.Time {
	if d, ok := ctx.Deadline(); ok {
		return d
	}
	return time.Now().Add(10 * time.Second)
}
