package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/coder/websocket"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
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

// cleanupConnection handles disconnection cleanup: removes the connection
// from the map, publishes a disconnect event, and releases rate-limiter state.
func (r *Receiver) cleanupConnection(vc *vehicleConn) {
	vc.cancel()
	// Only remove from the map if this is still the active connection for
	// this VIN. A reconnecting vehicle may have already replaced the entry.
	if r.connections.CompareAndDelete(vc.vin, vc) {
		r.connCount.Add(-1)
		r.metrics.SetConnectedVehicles(int(r.connCount.Load()))
		r.rateLimiter.remove(vc.vin)
	}

	redacted := redactVIN(vc.vin)
	r.logger.Info("vehicle disconnected",
		slog.String("vin", redacted),
		slog.Duration("session", time.Since(vc.connected)),
	)

	// Use a detached context since the connection context is cancelled.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r.publishConnectivity(ctx, vc.vin, events.StatusDisconnected)
}

// publishConnectivity publishes a ConnectivityEvent to the event bus.
func (r *Receiver) publishConnectivity(ctx context.Context, vin string, status events.ConnectivityStatus) {
	evt := events.ConnectivityEvent{
		VIN:       vin,
		Status:    status,
		Timestamp: time.Now(),
	}

	if err := r.bus.Publish(ctx, events.NewEvent(evt)); err != nil {
		r.logger.Error("publish connectivity event failed",
			slog.String("vin", redactVIN(vin)),
			slog.String("status", status.String()),
			slog.Any("error", err),
		)
	}
}

// Shutdown gracefully closes all active vehicle connections. It waits for
// handlers to complete up to the context deadline.
func (r *Receiver) Shutdown(ctx context.Context) {
	r.logger.Info("receiver shutting down")

	r.connections.Range(func(key, value any) bool {
		vc := value.(*vehicleConn)
		_ = vc.conn.Close(websocket.StatusGoingAway, "server shutting down")
		vc.cancel()
		return true
	})

	// Wait briefly for handlers to complete cleanup.
	deadline := time.After(time.Until(deadlineFromCtx(ctx)))
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()

	for {
		if r.connCount.Load() == 0 {
			r.logger.Info("all vehicle connections closed")
			return
		}
		select {
		case <-deadline:
			r.logger.Warn("shutdown deadline reached with active connections",
				slog.Int("remaining", int(r.connCount.Load())),
			)
			return
		case <-tick.C:
		}
	}
}

// ConnectedVehicles returns the number of currently connected vehicles.
func (r *Receiver) ConnectedVehicles() int {
	return int(r.connCount.Load())
}
