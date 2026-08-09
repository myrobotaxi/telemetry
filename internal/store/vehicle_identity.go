// VIN-derived model / model-year write path (MYR-507).
//
// This is a NARROW, owner-scoped UPDATE against the Prisma-owned "Vehicle"
// table — the FIFTH sanctioned Go-side write carve-out on that table, after the
// MYR-257 owner-onboarding provision (`store.OwnerProvisioner`), the MYR-258
// owner-offboarding teardown (`store.OwnerTeardown`), the MYR-286 owner-entered
// license plate (`VehicleRepo.UpdateLicensePlate`) and the MYR-320 exterior
// colour (`VehicleRepo.UpdateVehicleColor`). Documented as such in
// docs/contracts/data-lifecycle.md §1.4.
//
// Why a carve-out is required at all: exactly the argument MYR-320 made for
// `color`, one field over. `model` and `year` are ALREADY wire fields on both
// the snapshot and the vehicles-list, and both are ALREADY emitted straight from
// these columns — they are `required` in vehicle-summary.schema.json and have
// been since v1. The columns have simply never been populated for a car the GO
// SERVER provisioned: `queryUpsertOwnedVehicle` seeds them `''` and `0` with a
// comment promising "the web sync / streaming pipeline fills real values later",
// and no such writer exists on either side. Cars linked through the Next.js
// Prisma flow do carry them, which is why the gap went unnoticed — the fleet
// looked half-correct.
//
// The field evidence: a shared car sat in production for hours reading `model`
// `''` / `year` `0`, so a rider's Live Activity could name it only by its
// colour ("UltraRed") and the review sheet fell back to the make ("Tesla").
// Writing these columns makes the value flow to every consumer with ZERO
// contract change — the same reasoning, and the same shape of fix, as the colour
// write it sits beside.
//
// Why CG-DL-9 does not fire: CG-DL-9 constrains Go *migration SQL* — a Go
// migration may not reference a Prisma-owned table. This ships NO migration
// (both columns already exist, NOT NULL, with the placeholders above).
// Application-runtime Prisma UPDATEs are the sanctioned class, exactly like the
// colour write, the plate write and OwnerProvisioner's runtime upsert.

package store

import (
	"context"
	"fmt"
)

// queryFillVehicleIdentity fills a vehicle's `model` / `year` columns from the
// VIN-derived values, for a single vehicle the given user OWNS.
//
// FILL-IF-EMPTY, NOT OVERWRITE, and the two arms are independent — a car may
// have a model and no year. The CASE arms leave a populated column exactly as
// it was, which matters because the Prisma web-link flow writes a RICHER model
// for some rows ("Model 3 Performance") than position 4 of the VIN can encode
// ("Model 3"). A blind overwrite would DOWNGRADE those rows: it would trade a
// real bug for a subtler one.
//
// The WHERE clause repeats the emptiness test so the statement is a genuine
// no-op — zero rows, no `updatedAt` bump — once there is nothing left to fill.
// That is load-bearing rather than tidy: the caller runs on every connectivity
// edge for every car in the fleet, and without this arm each pass would touch
// every "Vehicle" row and stream a pointless change to the Prisma side forever.
//
// Ownership is a WHERE-clause predicate, not a precondition the caller is
// trusted to have checked: a mismatched "userId" updates zero rows rather than
// another owner's car. Keying on "vin" pairs with the caller, which speaks VINs,
// and keeps the write fail-closed even if a VIN ever appeared on two rows.
//
// No telemetry column is touched, so this can never clobber the streaming
// pipeline's writes.
const queryFillVehicleIdentity = `
UPDATE "Vehicle"
SET "model"     = CASE WHEN "model" = '' THEN $1 ELSE "model" END,
    "year"      = CASE WHEN "year"  = 0  THEN $2 ELSE "year"  END,
    "updatedAt" = NOW()
WHERE "vin" = $3 AND "userId" = $4
  AND (("model" = '' AND $1 <> '') OR ("year" = 0 AND $2 <> 0))`

// FillVehicleIdentity fills the VIN-derived model and model year on a vehicle
// the given user owns, WITHOUT overwriting either column if it already holds a
// value.
//
// An EMPTY model and a ZERO year are a no-op returning (false, nil), NOT a
// clear — the same safety property as the sibling colour writer, enforced here
// as well as in the SQL. `internal/vin` returns those zero values for any VIN
// position it does not recognise, and "we could not read it" must never blank a
// value some other writer got right.
//
// Returns filled=false (and a nil error) when no row matched, which is the
// ORDINARY outcome and covers three indistinguishable cases: both columns were
// already populated (every subsequent call for the life of the car), the VIN is
// unknown, or the vehicle belongs to somebody else. The caller logs at Debug and
// moves on — identity is cosmetic and its absence must never disturb the
// control-field backfill it rides along with.
func (r *VehicleRepo) FillVehicleIdentity(
	ctx context.Context,
	userID, vin, model string,
	year int,
) (bool, error) {
	if model == "" && year == 0 {
		return false, nil
	}
	tag, err := r.pool.Exec(ctx, queryFillVehicleIdentity, model, year, vin, userID)
	if err != nil {
		// The VIN is P1 (data-classification.md §1.3) — redacted in the error.
		// The model and year are P0 and log-safe, but add nothing here.
		return false, fmt.Errorf("VehicleRepo.FillVehicleIdentity(user=%s, vin=%s): %w",
			userID, redactVIN(vin), err)
	}
	return tag.RowsAffected() > 0, nil
}
