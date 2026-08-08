// The ACCEPT dispatch-seam projection (MYR-175/176), split out of
// ride_request_owner_handler.go (MYR-383) to keep that file under the 300-line
// rule. Pure shape translation: it turns the accepted record into the
// internal-bus payload the nav-dispatch pipeline subscribes to. No decisions
// live here — every gate that could refuse the accept has already run.

package telemetry

import "github.com/myrobotaxi/telemetry/internal/events"

// buildRideAcceptedEvent projects the accepted record onto the dispatch-seam
// payload. Places travel plaintext on the internal bus (the repo already
// decrypted them).
//
// It deliberately does NOT copy rec.PassengerName / rec.PassengerPhone. The
// event dropped those fields in MYR-447 — see events.RideAcceptedEvent for why
// — and this is the assignment that used to put a passenger's name and phone
// on the bus on every accept. Nothing downstream lost a reader; there was none.
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
