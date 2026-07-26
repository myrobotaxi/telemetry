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
