package ws

import (
	"context"
	"log/slog"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
	"github.com/tnando/my-robo-taxi-telemetry/internal/mask"
)

// handleDriveUpdated accumulates GPS route points and periodically
// broadcasts the batch as a vehicle_update with routeCoordinates. This
// avoids flooding WebSocket clients with one message per GPS sample.
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

	// The live drive-route stream uses the wire field name
	// "routeCoordinates" (preserved from MYR-117 to avoid a
	// frontend-breaking rename). The canonical schema name in
	// vehicle-state.schema.json is "navRouteCoordinates"; both names
	// must be allow-listed in the mask for owners and viewers — see
	// internal/mask/tables.go.
	fields := map[string]any{
		"routeCoordinates": coordsToMapbox(points),
	}

	b.hub.BroadcastMasked(
		vehicleID,
		mask.ResourceVehicleState,
		time.Now().UTC().Format(time.RFC3339),
		fields,
	)
}
