package store

import (
	"errors"
	"fmt"

	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

var (
	// ErrVehicleNotFound is returned when a vehicle lookup finds no matching row.
	// Wraps sdk.ErrNotFound so callers can use errors.Is(err, sdk.ErrNotFound).
	ErrVehicleNotFound = fmt.Errorf("vehicle %w", sdk.ErrNotFound)

	// ErrDriveNotFound is returned when a drive lookup finds no matching row.
	// Wraps sdk.ErrNotFound so callers can use errors.Is(err, sdk.ErrNotFound).
	ErrDriveNotFound = fmt.Errorf("drive %w", sdk.ErrNotFound)

	// ErrRideRequestNotFound is returned when a ride-request lookup (or a
	// conditional update, e.g. resolving a reschedule that was never
	// proposed) matches no row.
	// Wraps sdk.ErrNotFound so callers can use errors.Is(err, sdk.ErrNotFound).
	ErrRideRequestNotFound = fmt.Errorf("ride request %w", sdk.ErrNotFound)

	// ErrRideRequestConflict is returned by UpdateStatusFrom when the row
	// exists but its current status is not in the caller's allowed-from set
	// — the transition lost a race (or was illegal to begin with). The HTTP
	// layer maps it to 409 conflict (rest-api.md §7.8). Deliberately does
	// NOT wrap sdk.ErrNotFound.
	ErrRideRequestConflict = errors.New("ride request status conflict")

	// ErrRideRequestReservationDormant is returned by UpdateStatusFromDispatched
	// when the guarded write matched no row because the ride is a RESERVATION
	// THAT IS NEITHER DISPATCHED NOR YET DUE (MYR-376): `scheduled_for IS NOT
	// NULL`, `dispatch_status` is anything other than `'sent'`, AND
	// `scheduled_for` is still in the future. A reservation is DORMANT between
	// accept and the earlier of its dispatch and its due instant — before then
	// the car has been told nothing — so the owner pickup transition refuses it.
	// At/after due the refusal lifts even without a dispatch, which is what keeps
	// §7.8's "may still cancel or proceed manually" expiry promise true.
	//
	// The HTTP layer maps it to the SAME `409 conflict` code as
	// ErrRideRequestConflict (rest-api.md §7.8 — no new error code); the two are
	// separate sentinels only so the 409 can NAME the real reason instead of
	// blaming the status. Deliberately does NOT wrap sdk.ErrNotFound.
	ErrRideRequestReservationDormant = errors.New("ride request reservation not yet dispatched")

	// ErrRideRequestActive is returned by Create when the rider already has
	// an OPEN instant ride request and the partial unique index
	// (uq_go_ride_requests_active_instant_rider, migration 0004) rejects the
	// second INSERT with 23505. The HTTP layer maps it to 409 `ride_active`
	// (rest-api.md §7.8) and fetches the existing open request (GetActive
	// InstantByRider) for the response body. Deliberately does NOT wrap
	// sdk.ErrNotFound — it is a conflict, not a missing resource.
	ErrRideRequestActive = errors.New("ride request already active")

	// ErrVehicleRideActive is returned by UpdateStatusFrom when the guarded
	// requested->accepted transition is rejected because the target VEHICLE is
	// already committed to another active instant ride — the partial unique
	// index (uq_go_ride_requests_active_instant_vehicle, migration 0013)
	// raises 23505 (MYR-266). The HTTP layer maps it to 409 (the accept path,
	// alongside the MYR-277 vehicle-unavailable gate). Deliberately does NOT
	// wrap sdk.ErrNotFound — it is a conflict, not a missing resource, and is
	// distinct from ErrRideRequestConflict (an illegal *transition* on THIS
	// ride): the transition is legal, the vehicle is just busy.
	ErrVehicleRideActive = errors.New("vehicle already on an active ride")

	// ErrRideWindowConflict is the sentinel every *RideWindowConflictError
	// unwraps to (MYR-383): the target VEHICLE is already promised to another
	// OPEN ride within RideConflictWindow of the requested instant, so it
	// cannot also serve this one. Raised at BOTH booking sites — the
	// reservation INSERT (Create) and the guarded requested->accepted UPDATE
	// (UpdateStatusFromUnconflicted) — each under the per-vehicle advisory
	// booking lock, so two concurrent conflicting bookings never both commit.
	//
	// The HTTP layer maps it to 409 `vehicle_unavailable` with
	// `subCode: time_conflict` (rest-api.md §7.8): like the MYR-277
	// in-service/offline gate and the MYR-266 per-vehicle busy guard, this is a
	// CAPABILITY refusal — the request is well formed and the caller
	// authorised, the car simply cannot serve it — not the `conflict` that
	// means an illegal lifecycle transition. Use errors.As with
	// *RideWindowConflictError to read the conflicting instant. Deliberately
	// does NOT wrap sdk.ErrNotFound.
	ErrRideWindowConflict = errors.New("vehicle already booked in this window")

	// ErrTeslaTokenNotFound is returned when no Tesla OAuth token exists
	// for a user in the Prisma-owned Account table.
	// Wraps sdk.ErrNotFound so callers can use errors.Is(err, sdk.ErrNotFound).
	ErrTeslaTokenNotFound = fmt.Errorf("tesla token %w", sdk.ErrNotFound)

	// ErrDatabaseClosed is returned when an operation is attempted on a
	// closed database connection pool.
	ErrDatabaseClosed = errors.New("database connection closed")
)

// redactVIN returns a VIN with only the last 4 characters visible.
// Used in error messages to avoid leaking full VINs into logs.
func redactVIN(vin string) string {
	if len(vin) <= 4 {
		return vin
	}
	return "***" + vin[len(vin)-4:]
}
