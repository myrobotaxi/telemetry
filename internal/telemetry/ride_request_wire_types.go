// The ride-request WIRE shapes — the JSON structs that cross the HTTP
// boundary, split out of ride_request_types.go (MYR-383) to keep that file
// under the 300-line rule. The source of truth for every field name and enum
// here is contracts schemas/ride-request.schema.json; these hand-roll it
// byte-for-byte (conformance CI checks it). Nothing but serialization lives
// here — the domain types, sentinels and store interface stay next door.

package telemetry

import "github.com/myrobotaxi/telemetry/internal/wserrors"

// ridePlaceWire is the wire RidePlace ($defs.RidePlace): lat/lng/label
// required, address omitted when absent.
type ridePlaceWire struct {
	Lat     float64 `json:"lat"`
	Lng     float64 `json:"lng"`
	Label   string  `json:"label"`
	Address *string `json:"address,omitempty"`
}

// rideRequestWire is the wire RideRequest object. Optional keys are omitted
// (omitempty) when their source pointer is nil, matching the schema's
// omit-when-absent convention.
type rideRequestWire struct {
	ID                    string        `json:"id"`
	RiderID               string        `json:"riderId"`
	OwnerID               string        `json:"ownerId"`
	VehicleID             string        `json:"vehicleId"`
	Pickup                ridePlaceWire `json:"pickup"`
	Dropoff               ridePlaceWire `json:"dropoff"`
	Status                string        `json:"status"`
	RequesterName         *string       `json:"requesterName,omitempty"`
	PassengerName         *string       `json:"passengerName,omitempty"`
	PassengerPhone        *string       `json:"passengerPhone,omitempty"`
	ScheduledFor          *string       `json:"scheduledFor,omitempty"`
	RescheduleProposedFor *string       `json:"rescheduleProposedFor,omitempty"`
	RescheduleStatus      *string       `json:"rescheduleStatus,omitempty"`
	AcceptedAt            *string       `json:"acceptedAt,omitempty"`
	CompletedAt           *string       `json:"completedAt,omitempty"`
	CreatedAt             string        `json:"createdAt"`
	UpdatedAt             string        `json:"updatedAt"`
	// Dispatch outcome (MYR-176) — optional, additive. Omitted until the
	// nav-dispatch push resolves (ride-request.schema.json $defs.RideRequest).
	DispatchStatus *string `json:"dispatchStatus,omitempty"`
	DispatchedAt   *string `json:"dispatchedAt,omitempty"`
	// CancelledBy names the party that initiated a cancellation — "rider" |
	// "owner" (MYR-522). Optional, additive; omitted on every non-cancelled
	// ride and on rides cancelled before the field existed (absence =
	// initiator unknown, consumers must not guess).
	CancelledBy *string `json:"cancelledBy,omitempty"`
}

// rideActiveErrorResponse is the 409 `ride_active` body (MYR-230). It carries
// the standard REST error envelope PLUS the rider's existing OPEN instant
// ride under `activeRideRequest` — the same RideRequest shape
// GET /api/ride-requests/{id} returns — so the SDK can adopt it into the
// pending/tracking UI instead of showing a decline or generic failure. The
// nested `error` object stays byte-compatible with every other error
// response (§4.1); `activeRideRequest` is an additive sibling emitted only
// for this code. Coordinates in the adopted ride are P1 and returned only to
// the ride's own rider (a party) — never logged (§4.1 rule 2).
type rideActiveErrorResponse struct {
	Error             wserrors.ErrorEnvelopeBody `json:"error"`
	ActiveRideRequest rideRequestWire            `json:"activeRideRequest"`
}

// rideRequestsPageResponse is the RideRequestsListResponse envelope. Mirrors
// drivesPageResponse: items always present (empty array, never null),
// nextCursor null on the final page.
type rideRequestsPageResponse struct {
	Items      []rideRequestWire `json:"items"`
	NextCursor *string           `json:"nextCursor"`
	HasMore    bool              `json:"hasMore"`
}

// rideRequestCreateBody is the POST /api/ride-requests request body
// (RideRequestCreateRequest). Pointers on pickup/dropoff so a missing key is
// distinguishable from a zero-value place for validation.
type rideRequestCreateBody struct {
	VehicleID      string         `json:"vehicleId"`
	Pickup         *ridePlaceWire `json:"pickup"`
	Dropoff        *ridePlaceWire `json:"dropoff"`
	PassengerName  *string        `json:"passengerName"`
	PassengerPhone *string        `json:"passengerPhone"`
	ScheduledFor   *string        `json:"scheduledFor"`
}
