// The reservation sweeper's one-read vehicle probe (MYR-372).

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// queryVehicleDispatchState reads, for one vehicle, the three facts the
// reservation sweeper must agree on before it claims: the persisted status, the
// owner's ride-sharing switch, and (MYR-581) whether the owner has a name at
// all.
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
// THE OWNER-NAME ARM (MYR-581) rides the same read, as the SAME
// `ownerNamedPredicate` expression the snapshot read and therefore both
// request-time gates use — one ladder, three enforcement layers, no second
// definition to drift. It reads the owner off the vehicle row this statement is
// already anchored on, so it needs no join and no extra parameter.
//
// The relation is spelled `"Vehicle"` rather than aliased `v` purely so the
// shared expression — which qualifies its subselects with `"Vehicle"."userId"`
// — drops in unchanged. An alias here would have forced a second, parameterized
// copy of the ladder, which is exactly the thing owner_name.go exists to
// prevent.
const queryVehicleDispatchState = `
SELECT "Vehicle"."status",
       COALESCE(gcs.ride_share_enabled, TRUE),
       ` + ownerNamedPredicate + `
FROM "Vehicle"
LEFT JOIN go_vehicle_control_state gcs ON gcs.vehicle_id = "Vehicle"."id"
WHERE "Vehicle"."id" = $1`

// GetDispatchState returns one vehicle's persisted status, its owner's
// ride-sharing switch, and whether that owner has a resolvable name, from a
// single statement.
//
// Unknown vehicle ids are ErrVehicleNotFound, NOT a fabricated pair. This is
// the opposite reading from RideShareEnabled taken alone, and deliberately so:
// that method probes a side table which knows nothing about which vehicles
// exist, whereas this one is anchored on the vehicle row itself, where absence
// is a real and reportable fact. The sweeper treats the error as "unknown,
// therefore hold", which is the recoverable answer either way.
func (r *VehicleRepo) GetDispatchState(ctx context.Context, vehicleID string) (DispatchState, error) {
	start := time.Now()
	var out DispatchState
	err := r.pool.QueryRow(ctx, queryVehicleDispatchState, vehicleID).
		Scan(&out.Status, &out.RideShareEnabled, &out.OwnerNamed)
	r.metrics.ObserveQueryDuration("vehicle.get_dispatch_state", time.Since(start).Seconds())
	if errors.Is(err, pgx.ErrNoRows) {
		r.metrics.IncQueryError("vehicle.get_dispatch_state")
		return DispatchState{}, fmt.Errorf("VehicleRepo.GetDispatchState(%s): %w", vehicleID, ErrVehicleNotFound)
	}
	if err != nil {
		r.metrics.IncQueryError("vehicle.get_dispatch_state")
		return DispatchState{}, fmt.Errorf("VehicleRepo.GetDispatchState(%s): %w", vehicleID, err)
	}
	return out, nil
}

// DispatchState is what one GetDispatchState read answers.
//
// A STRUCT as of MYR-581, replacing a `(VehicleStatus, bool, error)` triple. The
// third fact was the tipping point: two same-typed bools in a positional return
// are a transposition waiting to happen, and transposing THESE two would make a
// paused car dispatchable and a named owner's car un-dispatchable at the same
// time. Named fields foreclose it.
//
// Both booleans are stated POSITIVELY — true means the reservation may proceed
// on that axis — matching dispatch.VehicleDispatchState, so no caller has to
// keep an inverted flag straight across the adapter boundary.
type DispatchState struct {
	Status           VehicleStatus
	RideShareEnabled bool
	// OwnerNamed is false when the vehicle's owner has no resolvable display
	// name (MYR-581). The sweeper HOLDS on it rather than expiring.
	OwnerNamed bool
}
