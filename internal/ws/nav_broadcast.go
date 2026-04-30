package ws

import (
	"context"
	"log/slog"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
	"github.com/tnando/my-robo-taxi-telemetry/internal/mask"
)

// handleTelemetry transforms a VehicleTelemetryEvent into a vehicle_update
// message and broadcasts it to authorized clients. Fields are partitioned
// by atomic group (vehicle-state-schema.md §1.1, §2): navigation-group
// fields are routed through the groupAccumulator for batched delivery;
// charge/gps/gear-group fields and individual fields are broadcast
// immediately because their atomicity is guaranteed upstream (Tesla's
// 500 ms bucket for charge; co-emission for lat/lng; synchronous derivation
// for status).
func (b *Broadcaster) handleTelemetry(ctx context.Context, event events.Event) {
	payload, ok := event.Payload.(events.VehicleTelemetryEvent)
	if !ok {
		b.logger.Error("broadcaster.handleTelemetry: unexpected payload type",
			slog.String("event_id", event.ID),
		)
		return
	}

	// Partition fields: navigation group accumulates; everything else is
	// broadcast immediately.
	navFields := make(map[string]events.TelemetryValue)
	nonNavFields := make(map[string]events.TelemetryValue)
	for name, val := range payload.Fields {
		if g, inGroup := groupOf(name); inGroup && g == groupNavigation {
			navFields[name] = val
		} else {
			nonNavFields[name] = val
		}
	}

	// Route nav fields through the accumulator (batched after 500ms window).
	if len(navFields) > 0 {
		b.groups.Add(groupNavigation, payload.VIN, navFields)
	}

	// Broadcast non-nav fields immediately (speed, gear, battery, etc.).
	if len(nonNavFields) == 0 {
		return
	}

	vehicleID, err := b.resolver.GetByVIN(ctx, payload.VIN)
	if err != nil {
		b.logger.Warn("broadcaster.handleTelemetry: VIN resolution failed, skipping event",
			slog.String("event_id", event.ID),
			slog.Any("error", err),
		)
		return
	}

	fields := mapFieldsForClient(nonNavFields)
	if len(fields) == 0 {
		return
	}

	fields["lastUpdated"] = payload.CreatedAt.Format(time.RFC3339)
	if _, hasGear := fields["gearPosition"]; hasGear {
		fields["status"] = deriveVehicleStatus(fields)
	}

	// Per-role projection per websocket-protocol.md §4.6 — the hub
	// pre-marshals one frame per role using the v1 vehicle_state mask
	// matrix and fans out the role-appropriate bytes.
	b.hub.BroadcastMasked(
		vehicleID,
		mask.ResourceVehicleState,
		payload.CreatedAt.Format(time.RFC3339),
		fields,
	)
}

// flushGroup is the callback invoked by the groupAccumulator when an
// atomic group's time window expires for a VIN. It resolves the VIN to a
// vehicle ID, maps the accumulated fields for the client, and broadcasts
// a vehicle_update. In v1 only the navigation group flows through here;
// future groups can dispatch on the group parameter.
func (b *Broadcaster) flushGroup(group atomicGroupID, vin string, fields map[string]events.TelemetryValue) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	vehicleID, err := b.resolver.GetByVIN(ctx, vin)
	if err != nil {
		b.logger.Warn("broadcaster.flushGroup: VIN resolution failed, dropping batch",
			slog.String("group", string(group)),
			slog.Any("error", err),
		)
		return
	}

	clientFields := mapFieldsForClient(fields)
	if len(clientFields) == 0 {
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	clientFields["lastUpdated"] = now

	b.hub.BroadcastMasked(
		vehicleID,
		mask.ResourceVehicleState,
		now,
		clientFields,
	)
}
