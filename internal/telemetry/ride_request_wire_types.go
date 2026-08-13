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

// rideStopWire is the wire RideStop ($defs.RideStop, MYR-539): the place, the
// server-minted id, and the server-owned status. Position is not a field — the
// array index IS the travel order.
type rideStopWire struct {
	Place  ridePlaceWire `json:"place"`
	ID     string        `json:"id"`
	Status string        `json:"status"`
}

// rideStopPatchWire is one entry of the desired list a client submits
// ($defs.RideStopPatch). `id` absent = a new stop; `place` is required on every
// entry, including one that keeps an existing id.
type rideStopPatchWire struct {
	Place *ridePlaceWire `json:"place"`
	ID    *string        `json:"id"`
}

// rideRequestWire is the wire RideRequest object. Optional keys are omitted
// (omitempty) when their source pointer is nil, matching the schema's
// omit-when-absent convention.
type rideRequestWire struct {
	ID        string        `json:"id"`
	RiderID   string        `json:"riderId"`
	OwnerID   string        `json:"ownerId"`
	VehicleID string        `json:"vehicleId"`
	Pickup    ridePlaceWire `json:"pickup"`
	Dropoff   ridePlaceWire `json:"dropoff"`
	// Stops (MYR-539) — optional and additive, OMITTED when the trip has none
	// so every pre-MYR-539 ride serializes byte-identically. Absent or empty
	// means a plain two-endpoint trip, never "unknown".
	Stops                 []rideStopWire `json:"stops,omitempty"`
	Status                string         `json:"status"`
	RequesterName         *string        `json:"requesterName,omitempty"`
	PassengerName         *string        `json:"passengerName,omitempty"`
	PassengerPhone        *string        `json:"passengerPhone,omitempty"`
	ScheduledFor          *string        `json:"scheduledFor,omitempty"`
	RescheduleProposedFor *string        `json:"rescheduleProposedFor,omitempty"`
	RescheduleStatus      *string        `json:"rescheduleStatus,omitempty"`
	AcceptedAt            *string        `json:"acceptedAt,omitempty"`
	CompletedAt           *string        `json:"completedAt,omitempty"`
	CreatedAt             string         `json:"createdAt"`
	UpdatedAt             string         `json:"updatedAt"`
	// Dispatch outcome (MYR-176) — optional, additive. Omitted until the
	// nav-dispatch push resolves (ride-request.schema.json $defs.RideRequest).
	DispatchStatus *string `json:"dispatchStatus,omitempty"`
	DispatchedAt   *string `json:"dispatchedAt,omitempty"`
	// CancelledBy names the party that initiated a cancellation — "rider" |
	// "owner" (MYR-522). Optional, additive; omitted on every non-cancelled
	// ride and on rides cancelled before the field existed (absence =
	// initiator unknown, consumers must not guess).
	CancelledBy *string `json:"cancelledBy,omitempty"`
	// TripVersion (MYR-541): omitted at 0 — the contract reads absence as 0,
	// so never-edited rows serialize byte-identically to pre-MYR-541 ones.
	TripVersion int `json:"tripVersion,omitempty"`
	// GroupRide (MYR-540) — optional and additive, OMITTED when false, because
	// the contract's rule is that ABSENT MEANS FALSE. That is what keeps every
	// pre-MYR-540 row byte-identical without a rewrite, and it is why this is a
	// bare bool with omitempty rather than a pointer: there is no third state.
	GroupRide bool `json:"groupRide,omitempty"`
	// ShareURL is the complete signed join link (MYR-540), emitted only on an
	// accepted-or-later group ride whose code is still emittable — see
	// rideShareURL for the four presence rules.
	//
	// P1 AND BEARER, exactly as ShareInvite.shareUrl is: it CONTAINS the code,
	// so never log it, never put it in an error envelope, and deliver it only
	// on the party-scoped REST surfaces — never on a broadcast frame.
	ShareURL string `json:"shareUrl,omitempty"`
	// Members are the joiners riding along (MYR-540). OMITTED when there are
	// none, so an old solo ride needs no `members: []` to be correct; absent and
	// empty mean the same thing. The requester is deliberately NOT in the list.
	// P1 — every entry carries a person's first name.
	Members []rideMemberWire `json:"members,omitempty"`
}

// rideMemberWire is the wire RideMember ($defs.RideMember, MYR-540) —
// deliberately the smallest shape that lets a surface render the group: an
// identity to compare against your own account, and a first name to print.
// Both keys are REQUIRED, so neither carries omitempty.
type rideMemberWire struct {
	UserID    string `json:"userId"`
	FirstName string `json:"firstName"`
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
	// GroupRide (MYR-540) is the toggle the requester set before booking. A
	// pointer so an absent key is distinguishable from an explicit `false` at
	// the decode boundary — both mean the same thing here, but the create body
	// is DisallowUnknownFields-strict and a bare bool would have made an absent
	// key indistinguishable from a client that sent one it did not mean.
	GroupRide *bool `json:"groupRide"`
}
