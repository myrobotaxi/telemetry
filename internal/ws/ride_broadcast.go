package ws

import (
	"context"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// Ride-hailing broadcast handlers (P10, MYR-174). Unlike the drive/telemetry
// handlers these need no VIN resolution — the ride events already carry the
// vehicleId and the two party user ids. They transform the summary domain
// events into the summary-only WS frames and unicast them to the rider and
// vehicle-owner connections via Hub.SendToUsers (never a vehicle-keyed
// Broadcast, which would leak the ride to other shared viewers).

// handleRideRequestCreated transforms a RideRequestCreatedEvent into a
// ride_request_created frame and unicasts it to the two parties.
func (b *Broadcaster) handleRideRequestCreated(_ context.Context, event events.Event) {
	payload, ok := event.Payload.(events.RideRequestCreatedEvent)
	if !ok {
		b.logger.Error("broadcaster.handleRideRequestCreated: unexpected payload type",
			slog.String("event_id", event.ID),
		)
		return
	}

	msg, err := marshalWSMessage(msgTypeRideRequestCreated, rideRequestCreatedPayload{
		RideRequestID: payload.RideRequestID,
		VehicleID:     payload.VehicleID,
		RiderID:       payload.RiderID,
		Status:        payload.Status,
		ScheduledFor:  formatOptionalTime(payload.ScheduledFor),
		Timestamp:     payload.CreatedAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		b.logger.Error("broadcaster.handleRideRequestCreated: marshal failed",
			slog.String("event_id", event.ID),
			slog.Any("error", err),
		)
		return
	}

	b.hub.SendToUsers([]string{payload.RiderID, payload.OwnerID}, msg)
}

// handleRideStatusChanged transforms a RideStatusChangedEvent into a
// ride_status_changed frame and unicasts it to the two parties.
func (b *Broadcaster) handleRideStatusChanged(_ context.Context, event events.Event) {
	payload, ok := event.Payload.(events.RideStatusChangedEvent)
	if !ok {
		b.logger.Error("broadcaster.handleRideStatusChanged: unexpected payload type",
			slog.String("event_id", event.ID),
		)
		return
	}

	msg, err := marshalWSMessage(msgTypeRideStatusChanged, rideStatusChangedPayload{
		RideRequestID:    payload.RideRequestID,
		VehicleID:        payload.VehicleID,
		Status:           payload.Status,
		RescheduleStatus: payload.RescheduleStatus,
		Timestamp:        payload.UpdatedAt.UTC().Format(time.RFC3339),
	})
	if err != nil {
		b.logger.Error("broadcaster.handleRideStatusChanged: marshal failed",
			slog.String("event_id", event.ID),
			slog.Any("error", err),
		)
		return
	}

	b.hub.SendToUsers([]string{payload.RiderID, payload.OwnerID}, msg)
}

// formatOptionalTime renders a nullable timestamp as an RFC 3339 UTC string
// pointer, or nil when the input is nil — so the omitempty JSON tag drops
// the key entirely for on-demand rides / rides with no reschedule.
func formatOptionalTime(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}
