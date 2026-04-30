package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
)

// Broadcaster subscribes to event bus topics and transforms domain events
// into client-friendly JSON messages delivered through the Hub. It bridges
// the internal event system and the external WebSocket protocol.
type Broadcaster struct {
	hub      *Hub
	bus      events.Bus
	resolver VINResolver
	logger   *slog.Logger
	subs     []events.Subscription
	routes   *routeAccumulator
	groups   *groupAccumulator
}

// NewBroadcaster creates a Broadcaster ready to start. Call Start to begin
// subscribing to event bus topics.
func NewBroadcaster(hub *Hub, bus events.Bus, resolver VINResolver, logger *slog.Logger) *Broadcaster {
	b := &Broadcaster{
		hub:      hub,
		bus:      bus,
		resolver: resolver,
		logger:   logger,
		routes:   newRouteAccumulator(defaultRouteBatchSize, defaultRouteFlushInterval),
	}
	b.groups = newGroupAccumulator(defaultGroupFlushInterval, b.flushGroup)
	return b
}

// Start subscribes to all relevant event bus topics. The provided context
// is used for VIN resolution calls within event handlers.
func (b *Broadcaster) Start(ctx context.Context) error {
	type topicHandler struct {
		topic   events.Topic
		handler events.Handler
	}

	subscriptions := []topicHandler{
		{events.TopicVehicleTelemetry, b.makeHandler(b.handleTelemetry)},
		{events.TopicDriveStarted, b.makeHandler(b.handleDriveStarted)},
		{events.TopicDriveUpdated, b.makeHandler(b.handleDriveUpdated)},
		{events.TopicDriveEnded, b.makeHandler(b.handleDriveEnded)},
		{events.TopicConnectivity, b.makeHandler(b.handleConnectivity)},
	}

	for _, th := range subscriptions {
		sub, err := b.bus.Subscribe(th.topic, th.handler)
		if err != nil {
			// Unsubscribe any already-registered subscriptions on failure.
			b.unsubscribeAll()
			return fmt.Errorf("broadcaster.Start(topic=%s): %w", th.topic, err)
		}
		b.subs = append(b.subs, sub)
	}

	b.logger.Info("broadcaster started",
		slog.Int("subscriptions", len(b.subs)),
	)
	return nil
}

// Stop unsubscribes from all event bus topics and cancels any pending
// atomic-group accumulator timers. After Stop returns, no further events
// will be processed and no timer callbacks will fire.
func (b *Broadcaster) Stop() error {
	b.unsubscribeAll()
	b.groups.Stop()
	b.logger.Info("broadcaster stopped")
	return nil
}

// eventHandler is the internal signature for typed event processing
// functions that need a context for VIN resolution.
type eventHandler func(ctx context.Context, event events.Event)

// makeHandler wraps a context-aware event handler into the events.Handler
// signature expected by the bus. Each invocation gets a fresh 30s context
// so handlers are not affected by the parent context's lifetime.
func (b *Broadcaster) makeHandler(fn eventHandler) events.Handler {
	return func(event events.Event) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		fn(ctx, event)
	}
}

// handleDriveStarted transforms a DriveStartedEvent into a drive_started
// message and broadcasts it.
func (b *Broadcaster) handleDriveStarted(ctx context.Context, event events.Event) {
	payload, ok := event.Payload.(events.DriveStartedEvent)
	if !ok {
		b.logger.Error("broadcaster.handleDriveStarted: unexpected payload type",
			slog.String("event_id", event.ID),
		)
		return
	}

	vehicleID, err := b.resolver.GetByVIN(ctx, payload.VIN)
	if err != nil {
		b.logger.Warn("broadcaster.handleDriveStarted: VIN resolution failed, skipping event",
			slog.String("event_id", event.ID),
			slog.Any("error", err),
		)
		return
	}

	msg, err := marshalWSMessage(msgTypeDriveStarted, driveStartedPayload{
		VehicleID: vehicleID,
		DriveID:   payload.DriveID,
		StartLocation: startLocation{
			Latitude:  payload.Location.Latitude,
			Longitude: payload.Location.Longitude,
		},
		Timestamp: payload.StartedAt.Format(time.RFC3339),
	})
	if err != nil {
		b.logger.Error("broadcaster.handleDriveStarted: marshal failed",
			slog.String("event_id", event.ID),
			slog.Any("error", err),
		)
		return
	}

	b.hub.Broadcast(vehicleID, msg)
}

// handleDriveEnded transforms a DriveEndedEvent into a drive_ended
// message and broadcasts it. It also flushes any remaining accumulated
// route points and clears the accumulator for the vehicle.
func (b *Broadcaster) handleDriveEnded(ctx context.Context, event events.Event) {
	payload, ok := event.Payload.(events.DriveEndedEvent)
	if !ok {
		b.logger.Error("broadcaster.handleDriveEnded: unexpected payload type",
			slog.String("event_id", event.ID),
		)
		return
	}

	vehicleID, err := b.resolver.GetByVIN(ctx, payload.VIN)
	if err != nil {
		b.logger.Warn("broadcaster.handleDriveEnded: VIN resolution failed, skipping event",
			slog.String("event_id", event.ID),
			slog.Any("error", err),
		)
		return
	}

	// Flush any remaining route points before sending drive_ended.
	if remaining := b.routes.Flush(payload.VIN); len(remaining) > 0 {
		b.broadcastRoutePoints(ctx, event.ID, payload.VIN, remaining)
	}
	b.routes.Clear(payload.VIN)

	// Flush any pending nav fields for this VIN. Flush cancels the timer
	// and clears state, so a separate Clear call is unnecessary.
	if navFields := b.groups.Flush(groupNavigation, payload.VIN); len(navFields) > 0 {
		b.flushGroup(groupNavigation, payload.VIN, navFields)
	}

	msg, err := marshalWSMessage(msgTypeDriveEnded, driveEndedPayload{
		VehicleID:       vehicleID,
		DriveID:         payload.DriveID,
		Distance:        payload.Stats.Distance,
		DurationSeconds: payload.Stats.Duration.Seconds(),
		AvgSpeed:        payload.Stats.AvgSpeed,
		MaxSpeed:        payload.Stats.MaxSpeed,
		Timestamp:       payload.EndedAt.Format(time.RFC3339),
	})
	if err != nil {
		b.logger.Error("broadcaster.handleDriveEnded: marshal failed",
			slog.String("event_id", event.ID),
			slog.Any("error", err),
		)
		return
	}

	b.hub.Broadcast(vehicleID, msg)
}

// handleConnectivity transforms a ConnectivityEvent into a connectivity
// message and broadcasts it.
func (b *Broadcaster) handleConnectivity(ctx context.Context, event events.Event) {
	payload, ok := event.Payload.(events.ConnectivityEvent)
	if !ok {
		b.logger.Error("broadcaster.handleConnectivity: unexpected payload type",
			slog.String("event_id", event.ID),
		)
		return
	}

	vehicleID, err := b.resolver.GetByVIN(ctx, payload.VIN)
	if err != nil {
		b.logger.Warn("broadcaster.handleConnectivity: VIN resolution failed, skipping event",
			slog.String("event_id", event.ID),
			slog.Any("error", err),
		)
		return
	}

	msg, err := marshalWSMessage(msgTypeConnectivity, connectivityPayload{
		VehicleID: vehicleID,
		Online:    payload.Status == events.StatusConnected,
		Timestamp: payload.Timestamp.Format(time.RFC3339),
	})
	if err != nil {
		b.logger.Error("broadcaster.handleConnectivity: marshal failed",
			slog.String("event_id", event.ID),
			slog.Any("error", err),
		)
		return
	}

	b.hub.Broadcast(vehicleID, msg)

	// Clear pending nav fields when vehicle disconnects to avoid
	// broadcasting stale navigation data on reconnect.
	if payload.Status == events.StatusDisconnected {
		b.groups.Clear(groupNavigation, payload.VIN)
	}
}

// unsubscribeAll removes all active subscriptions from the bus.
func (b *Broadcaster) unsubscribeAll() {
	for _, sub := range b.subs {
		if err := b.bus.Unsubscribe(sub); err != nil {
			b.logger.Warn("broadcaster.unsubscribeAll: failed to unsubscribe",
				slog.String("subscription_id", sub.ID),
				slog.Any("error", err),
			)
		}
	}
	b.subs = nil
}

// marshalWSMessage creates a JSON-encoded WebSocket message envelope.
func marshalWSMessage(msgType string, payload any) ([]byte, error) {
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshalWSMessage(%s): marshal payload: %w", msgType, err)
	}

	msg, err := json.Marshal(wsMessage{
		Type:    msgType,
		Payload: payloadBytes,
	})
	if err != nil {
		return nil, fmt.Errorf("marshalWSMessage(%s): marshal envelope: %w", msgType, err)
	}
	return msg, nil
}
