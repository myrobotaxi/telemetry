// The reservation sweeper's one-read vehicle probe (MYR-372).

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// queryVehicleDispatchState reads, for one vehicle, the two facts the
// reservation sweeper must agree on before it claims: the persisted status and
// the owner's ride-sharing switch.
//
// A READ of the Prisma-owned "Vehicle" table on its primary key, LEFT JOINed to
// the Go-owned control-state side table — the same join shape queryVehicleByID
// uses, narrowed from thirty-one columns to two. ONE statement rather than two
// round trips is the point: the accept gate deliberately resolves everything it
// needs from a single row so its facts cannot disagree, and the unattended path
// should hold itself to no less.
//
// The COALESCE carries queryRideShareEnabled's invariant unchanged: a car with
// no side-table row is ENABLED, not unknown. Without it a missing row would
// scan SQL NULL and every caller would collapse it to true anyway — inviting
// exactly one caller to forget.
//
// Deliberately NOT `... AND status NOT IN ('in_service','offline')`. Which
// statuses block a dispatch is a POLICY, spelled once in internal/telemetry's
// vehicleAvailability, and the two readers of it act on DIFFERENT arms — the
// accept gate on both, the sweeper on `in_service` only. A SQL literal here
// would be a second definition that could not express that difference and that
// nobody would think to update.
const queryVehicleDispatchState = `
SELECT v."status",
       COALESCE(gcs.ride_share_enabled, TRUE)
FROM "Vehicle" v
LEFT JOIN go_vehicle_control_state gcs ON gcs.vehicle_id = v."id"
WHERE v."id" = $1`

// GetDispatchState returns one vehicle's persisted status and its owner's
// ride-sharing switch, from a single statement.
//
// Unknown vehicle ids are ErrVehicleNotFound, NOT a fabricated pair. This is
// the opposite reading from RideShareEnabled taken alone, and deliberately so:
// that method probes a side table which knows nothing about which vehicles
// exist, whereas this one is anchored on the vehicle row itself, where absence
// is a real and reportable fact. The sweeper treats the error as "unknown,
// therefore hold", which is the recoverable answer either way.
func (r *VehicleRepo) GetDispatchState(ctx context.Context, vehicleID string) (VehicleStatus, bool, error) {
	start := time.Now()
	var (
		status           VehicleStatus
		rideShareEnabled bool
	)
	err := r.pool.QueryRow(ctx, queryVehicleDispatchState, vehicleID).Scan(&status, &rideShareEnabled)
	r.metrics.ObserveQueryDuration("vehicle.get_dispatch_state", time.Since(start).Seconds())
	if errors.Is(err, pgx.ErrNoRows) {
		r.metrics.IncQueryError("vehicle.get_dispatch_state")
		return "", false, fmt.Errorf("VehicleRepo.GetDispatchState(%s): %w", vehicleID, ErrVehicleNotFound)
	}
	if err != nil {
		r.metrics.IncQueryError("vehicle.get_dispatch_state")
		return "", false, fmt.Errorf("VehicleRepo.GetDispatchState(%s): %w", vehicleID, err)
	}
	return status, rideShareEnabled, nil
}
