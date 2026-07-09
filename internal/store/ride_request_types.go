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

	Status RideRequestStatus

	// Booked-for passenger ("Someone else" rides). Both nil when the rider
	// is riding themselves.
	PassengerName  *string
	PassengerPhone *string

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
}
