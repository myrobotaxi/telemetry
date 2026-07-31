package telemetry

import (
	"context"
	"errors"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// Ride-request HTTP surface types (P10 ride-hailing, MYR-174). The wire
// source of truth is contracts schemas/ride-request.schema.json; these Go
// shapes hand-roll the same field names/enums byte-for-byte (conformance CI
// checks this). Handler-local domain types + a cmd-side adapter keep the
// handler decoupled from internal/store (same pattern as the drives handler).

// RidePlaceData is a pickup or drop-off point in the handler layer. Address
// is nil when the place carried a label only (pin-drops, "Current location").
type RidePlaceData struct {
	Latitude  float64
	Longitude float64
	Label     string
	Address   *string
}

// RideRequestCreateInput is what the handler hands the store on create. The
// rider/owner ids are server-derived (rider = JWT sub; owner = the vehicle's
// owner), never client-supplied.
type RideRequestCreateInput struct {
	RiderID        string
	OwnerID        string
	VehicleID      string
	Pickup         RidePlaceData
	Dropoff        RidePlaceData
	PassengerName  *string
	PassengerPhone *string
	ScheduledFor   *time.Time
}

// RideRequestData is the full ride-request aggregate the store returns and
// the handler projects onto the wire RideRequest object.
type RideRequestData struct {
	ID                    string
	RiderID               string
	OwnerID               string
	VehicleID             string
	Pickup                RidePlaceData
	Dropoff               RidePlaceData
	Status                string
	PassengerName         *string
	PassengerPhone        *string
	ScheduledFor          *time.Time
	RescheduleProposedFor *time.Time
	RescheduleStatus      *string
	AcceptedAt            *time.Time
	CompletedAt           *time.Time
	CreatedAt             time.Time
	UpdatedAt             time.Time

	// RequesterName is the requester's resolved display name (MYR-229),
	// populated server-side from the rider's identity. nil = the field is
	// OMITTED on the wire (the rider has no identity row); never an empty
	// string. P1 PII — never logged.
	RequesterName *string

	// Dispatch outcome (MYR-176). Nil until the nav-dispatch push resolves.
	// Surfaced on the party-only detail payload (dispatchStatus/dispatchedAt);
	// the internal error code is not exposed on the wire.
	DispatchStatus *string
	DispatchedAt   *time.Time

	// DispatchError is the opaque reason code recorded alongside a `failed`
	// DispatchStatus. SERVER-SIDE ONLY — it is deliberately absent from the
	// wire projection in this file, and adding it there would be a contract
	// change, not a refactor.
	//
	// It is read for exactly one decision (MYR-172): `reservation_expired` is
	// how the sweeper records giving up on a late scheduled ride, and it is the
	// ONLY signal that the ride's Live Activity has been ended for good — the
	// sweeper leaves the ride's own status at `accepted`, so nothing else on
	// this record distinguishes an expired reservation from a live one.
	DispatchError *string
}

// BookedWindowData is one interval in which a vehicle cannot take a new
// reservation, as the MYR-385 picker read surface returns it.
//
// Start/End are CONCRETE instants the store has already resolved against the
// conflict half-width — the handler never adds or subtracts anything, and
// neither does the client. That is deliberate: the half-width is a product
// guess living in one place on the server, and emitting resolved endpoints is
// what keeps every picker in step with the gate when it moves.
//
// The interval is OPEN at both ends, inherited from the gate's strict
// comparison: a booking at exactly Start or exactly End is ALLOWED.
type BookedWindowData struct {
	Start   time.Time
	End     time.Time
	Pending bool
	Own     bool
}

// RideRequestListPage is one page of a keyset scan plus the has-more probe
// result.
type RideRequestListPage struct {
	Items   []RideRequestData
	HasMore bool
}

// ErrRideStatusConflict is returned by RideRequestStore.UpdateStatusFrom
// when the row exists but its current status is outside the allowed-from
// set — the transition lost a race or was illegal to begin with. The
// handlers map it to HTTP 409 conflict. The cmd adapter translates
// store.ErrRideRequestConflict into this sentinel so the handler layer
// stays decoupled from internal/store.
var ErrRideStatusConflict = errors.New("ride request status conflict")

// ErrRideReservationDormant is returned by
// RideRequestStore.UpdateStatusFromDispatched when the guarded write matched no
// row because the ride is a RESERVATION THAT IS NEITHER DISPATCHED NOR YET DUE
// (MYR-376) — `scheduledFor` is set, still in the future, and its dispatch has
// not resolved `sent`. A reservation is DORMANT between accept and the earlier
// of those two instants, so the owner pickup transition refuses it. At/after
// `scheduledFor` the refusal lifts even without a dispatch: §7.8 promises an
// expired reservation's parties may still proceed manually.
//
// The pickup handler maps it to the SAME HTTP 409 `conflict` code as
// ErrRideStatusConflict (no new error code — rest-api.md §7.8); it is a
// separate sentinel only so the message can name the real reason instead of
// blaming the status. The cmd adapter translates
// store.ErrRideRequestReservationDormant into it so the handler layer stays
// decoupled from internal/store.
var ErrRideReservationDormant = errors.New("ride request reservation not yet dispatched")

// ErrRideActive is returned by RideRequestStore.Create when the rider
// already has an OPEN instant ride request and the partial unique guard
// (migration 0004) rejects the second insert. The create handler maps it to
// HTTP 409 `ride_active` and fetches the existing open request (GetActive
// InstantByRider) for the response body. The cmd adapter translates
// store.ErrRideRequestActive into this sentinel so the handler layer stays
// decoupled from internal/store.
var ErrRideActive = errors.New("ride request already active")

// ErrVehicleRideActive is returned by RideRequestStore.UpdateStatusFrom when
// the guarded requested->accepted transition is rejected because the target
// VEHICLE is already committed to another active instant ride (the per-vehicle
// one-active-ride guard, migration 0013, MYR-266). The accept handler maps it
// to HTTP 409. The cmd adapter translates store.ErrVehicleRideActive into this
// sentinel so the handler layer stays decoupled from internal/store. Distinct
// from ErrRideStatusConflict (an illegal *transition* on THIS ride): the
// transition is legal, the vehicle is just busy.
var ErrVehicleRideActive = errors.New("vehicle already on an active ride")

// ErrRideWindowConflict is the sentinel every RideWindowConflictError unwraps to
// (MYR-383): the target VEHICLE is already promised to another OPEN ride within
// the booking window of the requested `scheduledFor`, so it cannot also serve
// this one. Raised by RideRequestStore.Create (rider-facing) and by
// UpdateStatusFromUnconflicted (the owner-accept backstop). The cmd adapter
// translates store.ErrRideWindowConflict into it so the handler layer stays
// decoupled from internal/store.
//
// Both handlers map it to 409 `vehicle_unavailable` with `subCode:
// time_conflict` — a CAPABILITY refusal like the MYR-277 in-service/offline
// gate, the MYR-342 pause and the MYR-266 busy guard, NOT the `conflict` that
// means an illegal lifecycle transition (the ride is perfectly legal; the car
// is spoken for at that hour).
var ErrRideWindowConflict = errors.New("vehicle already booked in this window")

// RideWindowConflictError carries the only details the refusal may disclose: WHEN
// the vehicle is already spoken for, and whether that claim is merely PENDING.
// ConflictAt is the conflicting ride's `scheduledFor`, or nil when the conflict
// is an ACTIVE INSTANT ride (happening now, with no scheduled instant to name).
// Pending is true when the conflicting reservation is still `requested` — it
// decides only which sentence the refusal says, never whether it refuses.
//
// Nothing else about the conflicting ride crosses this boundary — not its id,
// its rider, its requester name, its pickup or its dropoff. Those are P1 and
// belong to the other party (data-classification.md §1.9); the caller is not a
// party to that ride and asking about this car's availability must not become a
// way to read somebody else's calendar. The INSTANT alone is P0 operational
// timing — the same tier as `status` and as the MYR-316 service-window instant
// the sibling refusal already echoes — so naming it is safe, and it is exactly
// what the rider needs in order to pick a different time.
type RideWindowConflictError struct {
	ConflictAt *time.Time
	Pending    bool
}

func (e *RideWindowConflictError) Error() string { return ErrRideWindowConflict.Error() }

// Unwrap makes every RideWindowConflictError match errors.Is(err,
// ErrRideWindowConflict).
func (e *RideWindowConflictError) Unwrap() error { return ErrRideWindowConflict }

// RideRequestStore is the persistence surface the ride-request handlers
// need. Implemented by rideRequestStoreAdapter in cmd/telemetry-server over
// store.RideRequestRepo. MYR-174 uses Create/GetByID/UpdateStatusFrom/
// ListByRiderPage; MYR-175 (owner API) adds ListByOwnerPage.
type RideRequestStore interface {
	// Create inserts a new ride request. Returns ErrRideActive when the
	// rider already holds an OPEN instant ride (the one-active-ride guard,
	// MYR-230) — the partial unique index arbitrates the race, so two
	// concurrent instant creates never both succeed. Returns a
	// *RideWindowConflictError when the request is a RESERVATION whose target
	// vehicle is already promised to another open ride inside the booking
	// window (MYR-383) — arbitrated by a per-vehicle advisory lock held across
	// the check and the insert, so two concurrent conflicting reservations
	// never both succeed either.
	Create(ctx context.Context, in RideRequestCreateInput) (RideRequestData, error)
	GetByID(ctx context.Context, id string) (RideRequestData, error)
	// GetActiveInstantByRider returns the rider's single OPEN instant ride,
	// or an sdk.ErrNotFound-wrapping error when none is open. The create
	// handler uses it to populate the 409 `ride_active` body so the client
	// can adopt the existing ride.
	GetActiveInstantByRider(ctx context.Context, riderID string) (RideRequestData, error)
	// UpdateStatusFrom atomically transitions the ride to `to` ONLY when
	// its current status is in `from` (single guarded UPDATE — the
	// MYR-174/175 check-then-write race fix). Misses return
	// ErrRideStatusConflict (row exists, status outside `from`) or an
	// sdk.ErrNotFound-wrapping error (row gone).
	UpdateStatusFrom(ctx context.Context, id string, from []string, to string) (RideRequestData, error)
	// UpdateStatusFromDispatched is UpdateStatusFrom plus the MYR-376
	// RESERVATION DORMANCY precondition, carried in the SAME guarded UPDATE:
	// the row must also satisfy `scheduled_for IS NULL OR dispatch_status =
	// 'sent' OR scheduled_for <= NOW()`. It backs the owner pickup transition
	// (accepted → arrived) and nothing else. A reservation that is neither
	// dispatched nor yet due returns ErrRideReservationDormant; the other miss
	// outcomes are UpdateStatusFrom's (ErrRideStatusConflict, sdk.ErrNotFound).
	// INSTANT rides are unaffected — they satisfy the predicate whatever their
	// dispatch outcome — and so is any reservation past its due instant.
	UpdateStatusFromDispatched(ctx context.Context, id string, from []string, to string) (RideRequestData, error)
	// UpdateStatusFromUnconflicted is UpdateStatusFrom wrapped in the MYR-383
	// per-vehicle BOOKING LOCK, and it backs the owner ACCEPT transition
	// (requested → accepted) and nothing else. Inside one transaction it takes
	// the lock, re-reads what the ride promises, refuses a RESERVATION whose
	// window is already taken with a *RideWindowConflictError, and only then runs
	// the same guarded UPDATE. INSTANT accepts skip the window check entirely
	// (they still take the lock, so they serialize with reservation accepts on
	// the same car) and behave exactly like UpdateStatusFrom. The other miss
	// outcomes are UpdateStatusFrom's (ErrRideStatusConflict,
	// ErrVehicleRideActive, sdk.ErrNotFound).
	UpdateStatusFromUnconflicted(ctx context.Context, id string, from []string, to string) (RideRequestData, error)
	ListByRiderPage(ctx context.Context, riderID string, cursor RideRequestListCursor, limit int) (RideRequestListPage, error)
	ListByOwnerPage(ctx context.Context, ownerID string, status *string, cursor RideRequestListCursor, limit int) (RideRequestListPage, error)
	// ListUpcomingByOwnerVehiclePage returns the owner's ACCEPTED, still
	// FUTURE reservations for ONE vehicle, SOONEST first (MYR-360). Owner
	// scoping is the authorization model: a vehicle the caller does not own
	// matches no rows, so an unknown/unowned id is an empty page, never an
	// error the caller could read as "this vehicle exists".
	ListUpcomingByOwnerVehiclePage(ctx context.Context, ownerID, vehicleID string, cursor RideRequestUpcomingCursor, limit int) (RideRequestListPage, error)
	// ListBookedWindows returns the intervals in [from, to) in which the
	// vehicle is already spoken for, ordered by start (MYR-385) — the READ
	// side of the same rule Create enforces, derived from the same predicate
	// and the same window constant so a picker and the gate cannot drift.
	//
	// callerID decides the Own flag and NOTHING else: unlike
	// ListUpcomingByOwnerVehiclePage, ownership is NOT the authorization model
	// here (the surface is open to rides-tier viewers too), so the handler
	// runs the ride-CREATE capability check before calling this. An unknown or
	// free vehicle is an empty slice, never an error.
	ListBookedWindows(ctx context.Context, vehicleID, callerID string, from, to time.Time) ([]BookedWindowData, error)
}

// RideEventPublisher publishes the ride-hailing domain events onto the event
// bus. events.Bus satisfies it. The WS broadcaster turns the created/
// status-changed events into summary frames unicast to the two parties; the
// accept dispatch event (MYR-175) is the seam MYR-176 subscribes to. A
// publish failure is logged and swallowed by callers — the DB mutation has
// already committed, so a dropped notification must not fail the request
// (clients reconcile via the REST one-shot per FR-9.1/FR-9.2).
type RideEventPublisher interface {
	Publish(ctx context.Context, event events.Event) error
}

// ride-request lifecycle status constants (mirror the contracts
// RideRequestStatus enum, ride-request.schema.json $defs.RideRequestStatus).
const (
	rideStatusRequested = "requested"
	rideStatusAccepted  = "accepted"
	rideStatusDeclined  = "declined"
	rideStatusEnroute   = "enroute"
	rideStatusArrived   = "arrived"
	rideStatusCompleted = "completed"
	rideStatusCancelled = "cancelled"
)

// Ride-request list pagination bounds (rest-api.md §4.2.1, same envelope as
// the drives list).
const (
	rideListDefaultLimit = 20
	rideListMaxLimit     = 100
	rideListMinLimit     = 1
)
