// Tesla-sourced exterior-colour write path (MYR-320).
//
// This is a NARROW, owner-scoped UPDATE against the Prisma-owned "Vehicle"
// table — the FOURTH sanctioned Go-side write carve-out on that table, after the
// MYR-257 owner-onboarding provision (`store.OwnerProvisioner`), the MYR-258
// owner-offboarding teardown (`store.OwnerTeardown`) and the MYR-286
// owner-entered license plate (`VehicleRepo.UpdateLicensePlate`). It is
// documented as such in docs/contracts/data-lifecycle.md §1.4.
//
// Why a carve-out is required at all: `color` is ALREADY a wire field on both
// the snapshot and the vehicles-list, and it is ALREADY emitted straight from
// this column. The column has simply never been populated — the MYR-257
// provisioning INSERT seeds it with the empty placeholder `''` and no writer,
// Go or Prisma, has ever filled it in, so every consumer has always rendered a
// blank colour. Tesla does expose it, as
// `vehicle_data.vehicle_config.exterior_color` (live-verified: "Quicksilver"),
// which the Go server is the only reader of. Writing the column here therefore
// makes the value flow to every consumer with ZERO contract change; the
// alternative — a new wire field for a value the contract already ships — would
// be strictly worse.
//
// Why CG-DL-9 does not fire: CG-DL-9 constrains Go *migration SQL* — a Go
// migration may not reference a Prisma-owned table. This ships NO migration
// (the column already exists as `"color" TEXT NOT NULL`). Application-runtime
// Prisma UPDATEs are the sanctioned class, exactly like the plate write and
// OwnerProvisioner's runtime upsert.

package store

import (
	"context"
	"fmt"
)

// queryUpdateVehicleColor writes Tesla's exterior-colour name for a single
// vehicle the given user OWNS. Ownership is a WHERE-clause predicate, not a
// precondition the caller is trusted to have checked: a mismatched "userId"
// updates zero rows rather than another owner's car. Keying on "vin" (rather
// than the Prisma id) is deliberate — the caller is the telemetry monitor, which
// speaks VINs, and pairing the VIN with the owner keeps the write fail-closed
// even if a VIN ever appeared on more than one row.
//
// The bind is guarded non-empty by UpdateVehicleColor, so this statement can
// never blank a colour. "updatedAt" is bumped so the Prisma side observes the
// row as changed; no telemetry column is touched, so this can never clobber the
// streaming pipeline's writes.
const queryUpdateVehicleColor = `
UPDATE "Vehicle"
SET "color"     = $1,
    "updatedAt" = NOW()
WHERE "vin" = $2 AND "userId" = $3`

// UpdateVehicleColor sets the Tesla-reported exterior colour on a vehicle the
// given user owns.
//
// An EMPTY colour is a no-op returning (false, nil), NOT a clear. That is the
// whole safety property of this writer and it is enforced here as well as at the
// call site: Tesla omitting `exterior_color` from a partial `vehicle_data`
// payload must never blank a colour that an earlier read already got right. The
// column is NOT NULL and has only ever held either a real colour or the
// provisioning placeholder `''`, so "leave it alone" is always the correct
// response to having nothing to say.
//
// Returns updated=false (and a nil error) when no row matched, which the
// ownership-scoped WHERE makes indistinguishable between "unknown VIN" and
// "vehicle owned by someone else". The caller logs and moves on: a colour is
// cosmetic and its absence must never disturb the control-field backfill it
// rides along with.
func (r *VehicleRepo) UpdateVehicleColor(ctx context.Context, userID, vin, color string) (bool, error) {
	if color == "" {
		return false, nil
	}
	tag, err := r.pool.Exec(ctx, queryUpdateVehicleColor, color, vin, userID)
	if err != nil {
		// The VIN is P1 (data-classification.md §1.3) — redacted in the error.
		// The colour itself is P0 and log-safe, but adds nothing here.
		return false, fmt.Errorf("VehicleRepo.UpdateVehicleColor(user=%s, vin=%s): %w",
			userID, redactVIN(vin), err)
	}
	return tag.RowsAffected() > 0, nil
}
