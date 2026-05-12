package ws

import (
	"context"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/mask"
)

// handleDriveUpdated accumulates GPS route points and periodically
// broadcasts the batch as a vehicle_update with driveTrailCoordinates.
// This avoids flooding WebSocket clients with one message per GPS
// sample.
func (b *Broadcaster) handleDriveUpdated(ctx context.Context, event events.Event) {
	payload, ok := event.Payload.(events.DriveUpdatedEvent)
	if !ok {
		b.logger.Error("broadcaster.handleDriveUpdated: unexpected payload type",
			slog.String("event_id", event.ID),
		)
		return
	}

	coord := routeCoordinate{
		Latitude:  payload.RoutePoint.Latitude,
		Longitude: payload.RoutePoint.Longitude,
	}
	result := b.routes.Add(payload.VIN, coord)
	if !result.ShouldFlush {
		return
	}

	b.broadcastRoutePoints(ctx, event.ID, payload.VIN, result.Points)
}

// broadcastRoutePoints resolves VIN to vehicle ID and sends accumulated
// route coordinates as a vehicle_update message.
func (b *Broadcaster) broadcastRoutePoints(ctx context.Context, eventID, vin string, points []routeCoordinate) {
	vehicleID, err := b.resolver.GetByVIN(ctx, vin)
	if err != nil {
		b.logger.Warn("broadcaster.broadcastRoutePoints: VIN resolution failed, skipping batch",
			slog.String("event_id", eventID),
			slog.Any("error", err),
		)
		return
	}

	// driveTrailCoordinates carries the accumulated GPS trail of an
	// active drive ("where the car has been"). It is distinct from the
	// navigation atomic group's navRouteCoordinates, which carries
	// Tesla's planned route polyline ("where the car is going"). See
	// docs/contracts/websocket-protocol.md §4.1.6.
	fields := map[string]any{
		"driveTrailCoordinates": coordsToMapbox(points),
	}

	b.hub.BroadcastMasked(
		vehicleID,
		mask.ResourceVehicleState,
		time.Now().UTC().Format(time.RFC3339),
		fields,
	)
}
