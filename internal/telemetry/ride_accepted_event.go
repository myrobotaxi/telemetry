// The ACCEPT dispatch-seam projection (MYR-175/176), split out of
// ride_request_owner_handler.go (MYR-383) to keep that file under the 300-line
// rule. Pure shape translation: it turns the accepted record into the
// internal-bus payload the nav-dispatch pipeline subscribes to. No decisions
// live here — every gate that could refuse the accept has already run.

package telemetry

import "github.com/myrobotaxi/telemetry/internal/events"

// buildRideAcceptedEvent projects the accepted record onto the dispatch-seam
// payload. Places travel plaintext on the internal bus (the repo already
// decrypted them); PassengerName/Phone are flattened to empty strings when
// absent so MYR-176 can branch on emptiness without nil checks.
func buildRideAcceptedEvent(rec RideRequestData) events.RideAcceptedEvent {
	ev := events.RideAcceptedEvent{
		RideRequestID: rec.ID,
		VehicleID:     rec.VehicleID,
		RiderID:       rec.RiderID,
		OwnerID:       rec.OwnerID,
		Pickup:        toEventPlace(rec.Pickup),
		Dropoff:       toEventPlace(rec.Dropoff),
		ScheduledFor:  rec.ScheduledFor,
	}
	if rec.PassengerName != nil {
		ev.PassengerName = *rec.PassengerName
	}
	if rec.PassengerPhone != nil {
		ev.PassengerPhone = *rec.PassengerPhone
	}
	if rec.AcceptedAt != nil {
		ev.AcceptedAt = *rec.AcceptedAt
	} else {
		// UpdateStatus stamps accepted_at on first entry; fall back to
		// updated_at defensively so the event always carries an instant.
		ev.AcceptedAt = rec.UpdatedAt
	}
	return ev
}

// toEventPlace converts the handler place shape to the events-bus shape
// (Address flattened to "" when absent).
func toEventPlace(p RidePlaceData) events.RidePlace {
	out := events.RidePlace{
		Latitude:  p.Latitude,
		Longitude: p.Longitude,
		Label:     p.Label,
	}
	if p.Address != nil {
		out.Address = *p.Address
	}
	return out
}
