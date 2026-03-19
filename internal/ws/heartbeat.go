package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"
)

// heartbeatMessage is the pre-serialized heartbeat JSON sent to all
// clients. Computed once at startup to avoid repeated marshalling.
var heartbeatMessage = mustMarshal(wsMessage{Type: msgTypeHeartbeat})

// RunHeartbeat sends a heartbeat message to all connected clients at
// the given interval. It blocks until ctx is cancelled.
func (h *Hub) RunHeartbeat(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	h.logger.Info("heartbeat started", slog.Duration("interval", interval))

	for {
		select {
		case <-ctx.Done():
			h.logger.Info("heartbeat stopped")
			return
		case <-ticker.C:
			h.BroadcastAll(heartbeatMessage)
		}
	}
}

// mustMarshal marshals v to JSON and panics on failure. Used only for
// static message templates at init time.
func mustMarshal(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic("ws: failed to marshal static message: " + err.Error())
	}
	return data
}
