// Ride-request domain types (MYR-173). The wire source of truth is the
// contracts package (schemas/ride-request.schema.json); these structs are
// the persisted mirror backed by the Go-owned go_ride_requests table
// (migrations/0002_ride_requests.up.sql).
//
// Classification (docs/contracts/data-classification.md §1.9): pickup and
// dropoff coordinates are P1 GPS data — stored encrypt-only (AES-256-GCM,
// NFR-3.23) and never logged. Labels, addresses, and the booked-for
// passenger name/phone are P1 log-redacted. IDs, status, and timestamps
// are P0.

package store

import "time"

// RideRequestStatus mirrors the contracts RideRequestStatus wire enum —
// the main ride lifecycle. The reschedule negotiation is deliberately a
// separate sub-state (RescheduleStatus), not a member of this enum.
type RideRequestStatus string

const (
	RideRequestStatusRequested RideRequestStatus = "requested"
	RideRequestStatusAccepted  RideRequestStatus = "accepted"
	RideRequestStatusDeclined  RideRequestStatus = "declined"
	RideRequestStatusEnroute   RideRequestStatus = "enroute"
	RideRequestStatusArrived   RideRequestStatus = "arrived"
	RideRequestStatusCompleted RideRequestStatus = "completed"
	RideRequestStatusCancelled RideRequestStatus = "cancelled"
)

// RescheduleStatus mirrors the contracts RescheduleStatus wire enum — the
// reschedule-negotiation sub-state, orthogonal to RideRequestStatus. A nil
// *RescheduleStatus on RideRequestRecord means no reschedule was ever
// proposed for the ride.
type RescheduleStatus string

const (
	RescheduleStatusRequested RescheduleStatus = "requested"
	RescheduleStatusConfirmed RescheduleStatus = "confirmed"
	RescheduleStatusDeclined  RescheduleStatus = "declined"
)

// DispatchStatus is the outcome of the MYR-176 nav-dispatch push that fires
// once when an owner accepts a ride. It is an orthogonal annotation on the
// row — NOT a member of RideRequestStatus (accepted stays accepted). A nil
// *DispatchStatus on RideRequestRecord means dispatch has not resolved yet
// (or the ride was never accepted).
type DispatchStatus string

const (
	// DispatchStatusSent — the vehicle accepted the navigation_gps_request.
	DispatchStatusSent DispatchStatus = "sent"
	// DispatchStatusFailed — the push failed terminally or after exhausting
	// the bounded retry budget; DispatchError carries the opaque reason code.
	DispatchStatusFailed DispatchStatus = "failed"
	// DispatchStatusSkipped — the push was not attempted (kill-switch off).
	DispatchStatusSkipped DispatchStatus = "skipped"
)

// RidePlace is a pickup or drop-off point — contracts $defs.RidePlace.
// Latitude/Longitude are P1 GPS coordinates: the repo encrypts them into
// the *_enc columns on write and decrypts on read; they never touch the
// database (or logs) in plaintext.
type RidePlace struct {
	Latitude  float64
	Longitude float64
	Label     string
	Address   *string // nullable; secondary street-address line
}

// RideRequestRecord maps to the Go-owned go_ride_requests table and
// mirrors the contracts RideRequest object field-for-field (wire camelCase
// -> Go exported names; wire omit-when-absent -> nil pointers).
type RideRequestRecord struct {
	ID        string
	RiderID   string
	OwnerID   string
	VehicleID string

	Pickup  RidePlace
	Dropoff RidePlace

	// Stops are the ordered INTERMEDIATE stops between the two endpoints
	// (MYR-539), in travel order. Nil/empty is a plain two-endpoint trip —
	// which is every ride written before MYR-539. They live in their own table
	// (go_ride_stops) and are attached by a second query on the reads that
	// serve the wire RideRequest object; a lean read leaves this nil, and
	// nothing may read that as "this ride has no stops".
	Stops []RideStop

	Status RideRequestStatus

	// Booked-for passenger ("Someone else" rides). Both nil when the rider
	// is riding themselves.
	PassengerName  *string
	PassengerPhone *string

	// RequesterName is the requester's display name (MYR-229), resolved
	// server-side from the rider's identity via the Prisma-owned "User"
	// table: first name (first whitespace token of the display name) →
	// email local-part → "Rider". Empty string means the rider has no
	// "User" row at all — the wire projections OMIT the field in that case
	// (never emit an empty string). P1 PII — never logged.
	RequesterName string

	// Scheduled rides + reschedule negotiation. ScheduledFor nil = an
	// on-demand ("Now") ride. RescheduleProposedFor/RescheduleStatus nil =
	// no reschedule ever proposed.
	ScheduledFor          *time.Time
	RescheduleProposedFor *time.Time
	RescheduleStatus      *RescheduleStatus

	AcceptedAt  *time.Time
	CompletedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time

	// Dispatch outcome (MYR-176). All nil until the nav-dispatch push
	// resolves. DispatchStatus/DispatchError are set by RecordDispatchOutcome;
	// DispatchedAt is stamped at claim time by ClaimDispatch (it doubles as
	// the exactly-once latch). DispatchError is nil for a plain success, and
	// set whenever the outcome has a reason to name: every 'failed' outcome,
	// plus the one 'skipped' outcome that is not the kill-switch —
	// 'nav_superseded', a leg a later leg of the same ride took over (MYR-526).
	DispatchStatus *DispatchStatus
	DispatchedAt   *time.Time
	DispatchError  *string

	// CancelledBy records which PARTY initiated a cancellation — 'rider' or
	// 'owner' (MYR-522). Nil on every non-cancelled row, and on rows
	// cancelled before migration 0037: absence means "initiator unknown",
	// and consumers must not guess.
	CancelledBy *string
	// TripVersion is the monotonic version of the ride's trip shape (MYR-541):
	// bumped by every accepted trip edit, never by lifecycle transitions.
	TripVersion int

	// Group ride (MYR-540). GroupRide is the create-time flag; the other two
	// exist only once the OWNER HAS ACCEPTED a group ride, because that is when
	// the code is minted.
	//
	// JoinCode is P1 AND BEARER — a live credential for this ride's live
	// location. Never log it, never put it in an error message, and never
	// project it onto a wire shape: the only thing that crosses the HTTP
	// boundary is the SIGNED URL built from it, on party-scoped surfaces only.
	GroupRide         bool
	JoinCode          string
	JoinCodeExpiresAt *time.Time

	// Members are the joiners riding along (MYR-540), in join order. The
	// REQUESTER IS NOT ONE — they are RiderID. Like Stops they live in their own
	// table and are attached by a second query on the reads that serve the wire
	// RideRequest object; a lean read leaves this nil, and nothing may read that
	// as "this ride has no members".
	Members []RideMember
}

// RideMember is one joined member as the read path returns it — contracts
// $defs.RideMember, and deliberately the same two fields: an identity to
// compare against your own account and a first name to print.
//
// FirstName is DERIVED, never stored: the read resolves it through the same
// three-source identity ladder RequesterName uses (Prisma "User" → the Apple
// first-consent name → go_users, then first-name token → email local-part →
// "Rider"), so it is always non-empty and a member who renames themselves does
// not leave a stale copy in a ride's history. P1 PII — never logged.
type RideMember struct {
	UserID    string
	FirstName string
	JoinedAt  time.Time
}
