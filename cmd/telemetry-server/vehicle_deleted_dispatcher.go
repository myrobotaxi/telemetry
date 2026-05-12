package main

import (
	"context"
	"log/slog"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
	"github.com/myrobotaxi/telemetry/internal/ws"
)

// vehicleDeletedDispatcher fans a VehicleDeletedEvent out to every
// in-process consumer that needs to react: the WS hub (close
// subscribed clients), the Tesla mTLS receiver (close active inbound
// streams), the VIN cache (evict stale identifiers), and the JWT
// user-existence cache (force a refetch on the next handshake).
//
// Wiring lives at cmd/ rather than inside any internal/ package so
// the cleanup hook composes the in-process pieces without those
// pieces depending on each other (preserves the dependency-rule
// boundary in CLAUDE.md). The dispatcher only depends on the public
// API surface of each consumer.
type vehicleDeletedDispatcher struct {
	hub      *ws.Hub
	receiver *telemetry.Receiver
	vinCache *store.VINCache
	jwtAuth  *auth.JWTAuthenticator
	logger   *slog.Logger
}

// newVehicleDeletedDispatcher wires the dispatcher. Any of the
// consumers may be nil (e.g., dev mode with the NoopAuthenticator
// passes nil for jwtAuth). Each branch null-checks before acting.
func newVehicleDeletedDispatcher(
	hub *ws.Hub,
	receiver *telemetry.Receiver,
	vinCache *store.VINCache,
	jwtAuth *auth.JWTAuthenticator,
	logger *slog.Logger,
) *vehicleDeletedDispatcher {
	return &vehicleDeletedDispatcher{
		hub:      hub,
		receiver: receiver,
		vinCache: vinCache,
		jwtAuth:  jwtAuth,
		logger:   logger,
	}
}

// Subscribe registers the dispatcher on bus.TopicVehicleDeleted. The
// returned subscription is the caller's responsibility to unsubscribe
// on shutdown (in practice main.go relies on bus.Close to stop the
// goroutine).
func (d *vehicleDeletedDispatcher) Subscribe(bus events.Bus) (events.Subscription, error) {
	return bus.Subscribe(events.TopicVehicleDeleted, d.handle)
}

// handle is the events.Handler called once per VehicleDeletedEvent.
// The execution is sequential by design: the bus runs handlers in a
// dedicated per-subscriber goroutine, so we are free to call into
// each consumer in turn without spawning more goroutines.
func (d *vehicleDeletedDispatcher) handle(evt events.Event) {
	payload, ok := evt.Payload.(events.VehicleDeletedEvent)
	if !ok {
		d.logger.Warn("vehicle_deleted dispatcher: wrong payload type",
			slog.String("topic", string(evt.Topic)),
		)
		return
	}

	d.logger.Info("dispatching vehicle_deleted cleanup",
		slog.String("vehicle_id", payload.VehicleID),
		slog.String("user_id", payload.UserID),
		slog.Bool("has_vin", payload.VIN != ""),
	)

	// Order matters: invalidate caches BEFORE closing streams so a
	// reconnection that races the close path does not catch a stale
	// positive cache answer and slip back in. Eviction is cheap and
	// idempotent; the close path is the slow operation.
	//
	// 1. Evict the VIN cache so the next IsAuthorized lookup misses
	//    and the receiver rejects subsequent inbound frames for this
	//    VIN with HTTP 403.
	if d.vinCache != nil && payload.VIN != "" {
		d.vinCache.Invalidate(payload.VIN)
	}

	// 2. Drop the JWT user-existence cache entry so a deleted user's
	//    stale token is rejected on the next handshake. The trigger
	//    fires per-Vehicle, not per-User, so the same userID may be
	//    invalidated multiple times in one transaction; Invalidate
	//    is idempotent.
	if d.jwtAuth != nil && payload.UserID != "" {
		d.jwtAuth.InvalidateUser(payload.UserID)
	}

	// 3. Close WebSocket clients subscribed to this vehicle.
	if d.hub != nil {
		d.hub.RemoveVehicle(payload.VehicleID)
	}

	// 4. Close any active inbound Tesla mTLS stream. Skipped for
	//    pre-pairing vehicles (empty VIN) — there is no stream.
	if d.receiver != nil && payload.VIN != "" {
		d.receiver.RemoveStream(payload.VIN)
	}
}

// runNotifyListener starts the Postgres LISTEN goroutine. The
// listener owns its own dedicated pgx connection (separate from the
// request pool) and reconnects with exponential backoff on drops.
// Returns immediately; the listener runs until ctx is cancelled.
func runNotifyListener(ctx context.Context, dsn string, bus events.Bus, logger *slog.Logger) {
	listener := store.NewNotifyListener(
		store.NotifyListenerConfig{DatabaseURL: dsn},
		bus,
		logger.With(slog.String("component", "notify-listener")),
	)
	go func() {
		if err := listener.Run(ctx); err != nil {
			logger.Error("notify listener exited with error", slog.Any("error", err))
		}
	}()
}
