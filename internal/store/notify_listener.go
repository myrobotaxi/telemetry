package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
)

// VehicleDeletedChannel is the Postgres NOTIFY channel name the
// Next.js-owned trigger publishes on. Defined as a const so the same
// literal is shared between the LISTEN call and any test fixtures.
const VehicleDeletedChannel = "vehicle_deleted"

// vehicleDeletedPayload is the wire shape the Postgres trigger emits.
// JSON keys match the trigger SQL in the my-robo-taxi repo
// (prisma/migrations/.../vehicle_deleted_notify_trigger/migration.sql).
type vehicleDeletedPayload struct {
	VehicleID string `json:"vehicleId"`
	UserID    string `json:"userId"`
	VIN       string `json:"vin,omitempty"`
}

// NotifyListenerConfig holds tuning parameters for NotifyListener.
type NotifyListenerConfig struct {
	// DatabaseURL is the Postgres connection URL used to open the
	// dedicated long-lived listener connection. Should be the
	// non-pooled DIRECT_URL — PgBouncer transaction pooling is
	// incompatible with LISTEN, which is per-connection state.
	DatabaseURL string

	// ReconnectBackoff is the initial wait before retrying a dropped
	// listener connection. Doubles up to MaxReconnectBackoff on
	// repeated failures.
	ReconnectBackoff time.Duration

	// MaxReconnectBackoff caps the exponential backoff.
	MaxReconnectBackoff time.Duration
}

// NotifyListener subscribes to a Postgres LISTEN channel on a dedicated
// long-lived connection (separate from the request pool, since LISTEN
// holds the connection open) and republishes notifications onto the
// in-process event bus.
//
// Lifecycle:
//
//	listener := NewNotifyListener(cfg, bus, logger)
//	go listener.Run(ctx) // blocks until ctx is cancelled
//
// Failure semantics:
//
//   - On connection loss, the listener reconnects with exponential
//     backoff. Notifications fired during the reconnect window are
//     lost — accepted per the issue spec ("within seconds of the
//     Next.js commit"); the FR-10.1 cleanup is best-effort, not
//     guaranteed delivery.
//
//   - Malformed JSON payloads are logged at Warn and dropped — a bad
//     payload from a future trigger version must not crash the
//     listener loop.
//
//   - Empty VehicleID / UserID after parse is logged at Warn and
//     dropped (defensive: a trigger bug or schema drift should not
//     publish phantom events).
type NotifyListener struct {
	cfg    NotifyListenerConfig
	bus    events.Bus
	logger *slog.Logger
}

// NewNotifyListener constructs a NotifyListener. Defaults are applied
// for any zero-valued config fields.
func NewNotifyListener(cfg NotifyListenerConfig, bus events.Bus, logger *slog.Logger) *NotifyListener {
	if cfg.ReconnectBackoff <= 0 {
		cfg.ReconnectBackoff = time.Second
	}
	if cfg.MaxReconnectBackoff <= 0 {
		cfg.MaxReconnectBackoff = 30 * time.Second
	}
	return &NotifyListener{cfg: cfg, bus: bus, logger: logger}
}

// Run blocks until ctx is cancelled. It opens a dedicated pgx
// connection, issues `LISTEN vehicle_deleted`, then waits for
// notifications and publishes them as VehicleDeletedEvent on the bus.
// Connection drops are followed by an exponential-backoff reconnect.
func (l *NotifyListener) Run(ctx context.Context) error {
	backoff := l.cfg.ReconnectBackoff
	for {
		if err := ctx.Err(); err != nil {
			return nil //nolint:nilerr // ctx-cancellation is the clean-shutdown signal
		}

		err := l.runOnce(ctx)
		if err == nil {
			// Clean exit (ctx cancelled mid-loop).
			return nil
		}

		l.logger.Warn("notify listener: connection lost, reconnecting",
			slog.Duration("backoff", backoff),
			slog.Any("error", err),
		)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(backoff):
		}

		backoff *= 2
		if backoff > l.cfg.MaxReconnectBackoff {
			backoff = l.cfg.MaxReconnectBackoff
		}
	}
}

// runOnce opens a single listener connection and consumes
// notifications until the connection drops or ctx is cancelled.
// Returns nil only when ctx is cancelled cleanly; otherwise returns
// the underlying error so Run can decide whether to reconnect.
func (l *NotifyListener) runOnce(ctx context.Context) error {
	conn, err := pgx.Connect(ctx, l.cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("notify listener: connect: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()

	if _, err := conn.Exec(ctx, "LISTEN "+VehicleDeletedChannel); err != nil {
		return fmt.Errorf("notify listener: LISTEN %s: %w", VehicleDeletedChannel, err)
	}

	l.logger.Info("notify listener active",
		slog.String("channel", VehicleDeletedChannel),
	)

	for {
		notif, err := conn.WaitForNotification(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return fmt.Errorf("notify listener: WaitForNotification: %w", err)
		}

		l.handleNotification(ctx, notif.Channel, notif.Payload)
	}
}

// handleNotification parses one NOTIFY payload and publishes the
// corresponding VehicleDeletedEvent. Errors are logged and swallowed
// so a single bad payload does not halt the loop.
func (l *NotifyListener) handleNotification(ctx context.Context, channel, payload string) {
	if channel != VehicleDeletedChannel {
		l.logger.Warn("notify listener: ignoring unknown channel",
			slog.String("channel", channel),
		)
		return
	}

	var p vehicleDeletedPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		l.logger.Warn("notify listener: malformed payload, dropping",
			slog.String("payload", payload),
			slog.Any("error", err),
		)
		return
	}

	if p.VehicleID == "" || p.UserID == "" {
		l.logger.Warn("notify listener: empty identifiers, dropping",
			slog.String("vehicle_id", p.VehicleID),
			slog.String("user_id", p.UserID),
		)
		return
	}

	evt := events.VehicleDeletedEvent{
		VehicleID: p.VehicleID,
		UserID:    p.UserID,
		VIN:       p.VIN,
	}

	if err := l.bus.Publish(ctx, events.NewEvent(evt)); err != nil {
		l.logger.Error("notify listener: publish failed",
			slog.String("vehicle_id", p.VehicleID),
			slog.Any("error", err),
		)
		return
	}

	l.logger.Info("notify listener: vehicle_deleted dispatched",
		slog.String("vehicle_id", p.VehicleID),
		slog.String("user_id", p.UserID),
		slog.Bool("has_vin", p.VIN != ""),
	)
}
