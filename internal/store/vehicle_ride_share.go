// Owner ride-sharing switch, read + write path (MYR-342).
//
// `ride_share_enabled` on the Go-owned go_vehicle_control_state side table
// (migration 0021) joins the MYR-316 service-window pair as the only columns in
// that table with DEDICATED statements rather than a slot in ControlStateUpdate.
// The reason is the same one 0017's header gives, sharpened by the type.
//
// Every other column reaches the table through queryUpsertControlState, whose
// per-column form is `col = COALESCE(EXCLUDED.col, existing.col)`. That is
// exactly right for telemetry — a frame that does not carry a field must not
// erase it — but for THIS column it is actively wrong twice over:
//
//   - a NULL means "leave alone", so the upsert cannot express a write of
//     `false`, which is the entire point of the field: `false` is not the
//     absence of an opinion, it is the owner's decision;
//   - the column is NOT NULL, so it has no absence to encode in the first place.
//
// And there is a second, harder reason to keep it out of ControlStateUpdate:
// that struct is fed by TELEMETRY. If this column had a slot there, any frame
// flowing through the shared upsert could re-enable ride sharing on a car whose
// owner had paused it. Keeping the write on its own statement, reachable only
// from the owner-authenticated §7.18 handler, means the pause can be lifted by
// exactly one actor — the owner.
//
// Naming/ownership (CG-DL-9): this is the Go-owned side table, so unlike the
// MYR-286 plate writer no Prisma carve-out is involved. Ownership is NOT
// enforced in SQL here (the side table has no userId column) — the caller MUST
// verify it first, exactly as SetServiceExpectedEndAt requires.

package store

import (
	"context"
	"fmt"
)

// queryUpsertRideShareEnabled writes the owner's ride-sharing switch. Assigns
// UNCONDITIONALLY: both values are real decisions and each must overwrite the
// other.
//
// The INSERT arm is required because a car whose owner reaches for this toggle
// before any other control write has no side-table row yet. It writes the
// supplied value rather than leaning on the column DEFAULT, so the first thing
// an owner does can be to PAUSE — a DEFAULT-only INSERT would have created the
// row enabled and silently ignored them.
const queryUpsertRideShareEnabled = `
INSERT INTO go_vehicle_control_state (vehicle_id, ride_share_enabled, updated_at)
VALUES ($1, $2, NOW())
ON CONFLICT (vehicle_id) DO UPDATE
SET ride_share_enabled = EXCLUDED.ride_share_enabled,
    updated_at         = NOW()`

// queryRideShareEnabled reads the switch for one vehicle.
//
// The COALESCE over a scalar sub-select is what makes "no side-table row at
// all" and "row with the column at its default" indistinguishable, which is the
// invariant the whole feature rests on: a car that has never had a control
// write is ENABLED, not unknown. Without it the sub-select would return SQL
// NULL for a missing row and the scan would need a *bool that every caller
// collapsed to true anyway — inviting exactly one caller to forget.
//
// One probe of go_vehicle_control_state's vehicle_id PRIMARY KEY. The list and
// snapshot paths do NOT use this statement — they already LEFT JOIN the side
// table and carry the same COALESCE inline, so they pay nothing extra. This one
// exists for the two callers that hold a vehicle id and nothing else: the
// ride-request create gate and the reservation sweeper.
const queryRideShareEnabled = `
SELECT COALESCE(
    (SELECT ride_share_enabled FROM go_vehicle_control_state WHERE vehicle_id = $1),
    TRUE
)`

// SetRideShareEnabled records the owner's ride-sharing switch for a vehicle.
//
// Ownership is NOT enforced here: the side table has no userId column, so the
// caller (the PUT handler) MUST resolve and verify ownership against the
// Prisma-owned "Vehicle" row before calling. Unlike the plate writer there is no
// second SQL-layer check available to fall back on — this is the same standing
// warning SetServiceExpectedEndAt carries, and it matters more here, because a
// missed check would let a stranger switch somebody else's car off.
func (r *VehicleRepo) SetRideShareEnabled(ctx context.Context, vehicleID string, enabled bool) error {
	if _, err := r.pool.Exec(ctx, queryUpsertRideShareEnabled, vehicleID, enabled); err != nil {
		return fmt.Errorf("VehicleRepo.SetRideShareEnabled(%s): %w", vehicleID, err)
	}
	return nil
}

// RideShareEnabled reports whether the vehicle currently accepts ride requests.
//
// An unknown vehicle id is NOT an error and returns true: this statement reads
// the Go-owned side table, which knows nothing about which vehicles exist, and
// inventing a "not found" here would make every caller re-derive existence they
// have already established (the create gate resolved the vehicle to check
// ownership; the sweeper is holding a reservation that could only have been
// accepted for a real car). Existence is the callers' business; this answers
// only "is it paused".
func (r *VehicleRepo) RideShareEnabled(ctx context.Context, vehicleID string) (bool, error) {
	var enabled bool
	if err := r.pool.QueryRow(ctx, queryRideShareEnabled, vehicleID).Scan(&enabled); err != nil {
		return false, fmt.Errorf("VehicleRepo.RideShareEnabled(%s): %w", vehicleID, err)
	}
	return enabled, nil
}
