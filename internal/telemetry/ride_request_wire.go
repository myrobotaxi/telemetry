package telemetry

import (
	"fmt"
	"time"
)

// Ride-request wire projection + request-body validation (MYR-174). Kept
// separate from the handler so the byte-for-byte contract mapping
// (ride-request.schema.json) is reviewable in one place.

// toRideRequestWire projects the store-layer aggregate onto the wire
// RideRequest object. Optional timestamps/pointers become omitted keys when
// absent. All timestamps are RFC 3339 UTC.
//
// signer mints RideRequest.shareUrl (MYR-540) and may be nil — a deployment
// with no signing key omits the key rather than emitting an unsigned link the
// landing shell would bounce. `now` is passed rather than read here so the
// shareUrl presence rules, whose last clause is a linger past the ride's end,
// stay a pure function of their inputs and are table-testable at a fixed
// instant.
func toRideRequestWire(d RideRequestData, signer *InviteLinkSigner, now time.Time) rideRequestWire {
	return rideRequestWire{
		GroupRide:             d.GroupRide,
		ShareURL:              rideShareURL(d, signer, now),
		Members:               toRideMembersWire(d.Members),
		ID:                    d.ID,
		RiderID:               d.RiderID,
		OwnerID:               d.OwnerID,
		VehicleID:             d.VehicleID,
		Pickup:                toRidePlaceWire(d.Pickup),
		Dropoff:               toRidePlaceWire(d.Dropoff),
		Stops:                 toRideStopsWire(d.Stops),
		Status:                d.Status,
		RequesterName:         d.RequesterName,
		PassengerName:         d.PassengerName,
		PassengerPhone:        d.PassengerPhone,
		ScheduledFor:          formatRideTimePtr(d.ScheduledFor),
		RescheduleProposedFor: formatRideTimePtr(d.RescheduleProposedFor),
		RescheduleStatus:      d.RescheduleStatus,
		AcceptedAt:            formatRideTimePtr(d.AcceptedAt),
		CompletedAt:           formatRideTimePtr(d.CompletedAt),
		CreatedAt:             d.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:             d.UpdatedAt.UTC().Format(time.RFC3339),
		DispatchStatus:        d.DispatchStatus,
		DispatchedAt:          formatRideTimePtr(d.DispatchedAt),
		TripVersion:           d.TripVersion,
		CancelledBy:           d.CancelledBy,
	}
}

// toRideMembersWire projects the joiner list in join order (MYR-540). A ride
// with no joiners projects to nil — the omitted key, which the contract reads
// as "no joiners" exactly as it reads an empty array, and which keeps every ride
// written before group rides existed byte-identical on the wire.
func toRideMembersWire(members []RideMemberData) []rideMemberWire {
	if len(members) == 0 {
		return nil
	}
	out := make([]rideMemberWire, 0, len(members))
	for i := range members {
		out = append(out, rideMemberWire{
			UserID:    members[i].UserID,
			FirstName: members[i].FirstName,
		})
	}
	return out
}

// toRidePlaceWire projects a place; a nil/empty Address drops the key.
func toRidePlaceWire(p RidePlaceData) ridePlaceWire {
	w := ridePlaceWire{Lat: p.Latitude, Lng: p.Longitude, Label: p.Label}
	if p.Address != nil && *p.Address != "" {
		w.Address = p.Address
	}
	return w
}

// toRideStopsWire projects the stop list in travel order (MYR-539). A trip with
// no stops projects to nil — the omitted key, which the contract reads as "no
// stops" exactly as it reads an empty array, and which keeps every ride written
// before stops existed byte-identical on the wire.
func toRideStopsWire(stops []RideStopData) []rideStopWire {
	if len(stops) == 0 {
		return nil
	}
	out := make([]rideStopWire, 0, len(stops))
	for i := range stops {
		out = append(out, rideStopWire{
			Place:  toRidePlaceWire(stops[i].Place),
			ID:     stops[i].ID,
			Status: stops[i].Status,
		})
	}
	return out
}

// formatRideTimePtr renders a nullable timestamp as an RFC 3339 UTC string
// pointer, or nil (→ omitted key) when the input is nil.
func formatRideTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

// MaxScheduledForHorizon is how far into the future a ride may be booked
// (MYR-372). A `scheduledFor` beyond now + this is refused at create with
// 400 invalid_request; see rest-api.md §7.8.
//
// WHY THERE IS A BOUND AT ALL. Without one the field was unbounded, and a
// reservation is not a calendar entry — it is a standing instruction to drive
// somebody's car somewhere, held by a sweeper that re-evaluates it every 30
// seconds until it fires. A booking for the year 2400 is a row that is
// selected forever, an availability question nobody can answer, and a rider
// expectation nothing in the system intends to honour. Refusing it at the door
// is cheaper than carrying it.
//
// WHY 30 DAYS. It is a product guess, not a derived quantity, chosen to sit
// comfortably past every real booking anyone has described (a trip next week,
// an airport run next month) while cutting off the range where a reservation
// stops meaning anything. It is deliberately the ONLY place the number
// appears: no client, contract enum, migration or SQL literal encodes it, so
// moving it is a one-line change here plus the §7.8 sentence that quotes it.
//
// THE PAST IS DELIBERATELY UNBOUNDED. §7.8's accept-after-due semantics let an
// owner accept a reservation whose instant has already passed, and the
// sweeper's own lateness ceiling is what resolves those honestly. A lower
// bound here would refuse rides the contract already promises to take.
const MaxScheduledForHorizon = 30 * 24 * time.Hour

// validateCreateBody turns the decoded request body into a partial create
// input (rider/owner ids are filled by the handler) after enforcing the
// RideRequestCreateRequest schema constraints. Returns a human-readable
// reason string the handler emits as 400 invalid_request; the reason never
// contains P1 values (coordinates/labels/PII).
//
// now is passed in rather than read here so the horizon bound stays a pure
// function of its inputs — the whole validator is table-testable at a fixed
// instant, which a clock reached for inside it would not be.
//
//nolint:gocritic // the (input, reason) pair reads clearly at the single call site; a named 400-reason string beats an error wrapper here
func validateCreateBody(body rideRequestCreateBody, now time.Time) (RideRequestCreateInput, string) {
	if body.VehicleID == "" {
		return RideRequestCreateInput{}, "vehicleId is required"
	}

	pickup, reason := validateRidePlace("pickup", body.Pickup)
	if reason != "" {
		return RideRequestCreateInput{}, reason
	}
	dropoff, reason := validateRidePlace("dropoff", body.Dropoff)
	if reason != "" {
		return RideRequestCreateInput{}, reason
	}

	in := RideRequestCreateInput{
		VehicleID: body.VehicleID,
		Pickup:    pickup,
		Dropoff:   dropoff,
	}

	// groupRide (MYR-540): OPTIONAL, and an absent key is false — the contract's
	// own reading. There is nothing to validate; the flag says only what the
	// ride will do on accept, and no body in this contract changes it
	// afterwards.
	if body.GroupRide != nil {
		in.GroupRide = *body.GroupRide
	}

	// passengerName/passengerPhone: OPTIONAL, minLength 1 when present.
	if body.PassengerName != nil {
		if *body.PassengerName == "" {
			return RideRequestCreateInput{}, "passengerName must not be empty when present"
		}
		in.PassengerName = body.PassengerName
	}
	if body.PassengerPhone != nil {
		if *body.PassengerPhone == "" {
			return RideRequestCreateInput{}, "passengerPhone must not be empty when present"
		}
		in.PassengerPhone = body.PassengerPhone
	}

	if body.ScheduledFor != nil {
		ts, err := time.Parse(time.RFC3339, *body.ScheduledFor)
		if err != nil {
			return RideRequestCreateInput{}, "scheduledFor must be an RFC 3339 date-time"
		}
		utc := ts.UTC()
		// MYR-372 horizon bound. Strictly after the limit is refused, so a
		// booking exactly MaxScheduledForHorizon out is ALLOWED — the same
		// "equal is allowed" reading the service-window bound takes, and the
		// one that keeps a client rendering "30 days" from being refused by
		// its own arithmetic.
		if limit := now.Add(MaxScheduledForHorizon); utc.After(limit) {
			return RideRequestCreateInput{}, fmt.Sprintf(
				"scheduledFor must be no more than %d days in the future",
				int(MaxScheduledForHorizon/(24*time.Hour)),
			)
		}
		in.ScheduledFor = &utc
	}

	return in, ""
}

// validateRidePlace enforces the RidePlace schema: lat/lng/label required,
// lat in [-90,90], lng in [-180,180], label non-empty. The `field` prefix
// names which place failed without echoing coordinate values.
//
//nolint:gocritic // the (place, reason) pair reads clearly at the two call sites
func validateRidePlace(field string, p *ridePlaceWire) (RidePlaceData, string) {
	if p == nil {
		return RidePlaceData{}, fmt.Sprintf("%s is required", field)
	}
	if p.Lat < -90 || p.Lat > 90 {
		return RidePlaceData{}, fmt.Sprintf("%s.lat must be in [-90, 90]", field)
	}
	if p.Lng < -180 || p.Lng > 180 {
		return RidePlaceData{}, fmt.Sprintf("%s.lng must be in [-180, 180]", field)
	}
	if p.Label == "" {
		return RidePlaceData{}, fmt.Sprintf("%s.label is required", field)
	}
	out := RidePlaceData{Latitude: p.Lat, Longitude: p.Lng, Label: p.Label}
	if p.Address != nil && *p.Address != "" {
		out.Address = p.Address
	}
	return out, ""
}
