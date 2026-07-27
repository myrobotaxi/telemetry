// Owner-entered license-plate write path (MYR-286).
//
// This is a NARROW, owner-scoped UPDATE against the Prisma-owned "Vehicle"
// table — the third sanctioned Go-side write carve-out on that table, after the
// MYR-257 owner-onboarding provision (`store.OwnerProvisioner`) and the MYR-258
// owner-offboarding teardown (`store.OwnerTeardown`). It is documented as such
// in docs/contracts/data-lifecycle.md §1.4.
//
// Why a carve-out is required at all: the plate is NOT sourced from Tesla. The
// Fleet API exposes no plate on any endpoint, telemetry field, or proto, so the
// only way the column is ever populated is an owner typing it into a surface the
// Go server owns (PUT /api/tesla/vehicles/{vehicleId}/plate, rest-api.md §7.14).
// There is no Next.js/Prisma writer to defer to.
//
// Why CG-DL-9 does not fire: CG-DL-9 constrains Go *migration SQL* — a Go
// migration may not reference a Prisma-owned table. This ships NO migration
// (the column already exists as `"licensePlate" TEXT NOT NULL DEFAULT ''`).
// Application-runtime Prisma UPDATEs are the sanctioned class, exactly like
// OwnerProvisioner's runtime upsert.

package store

import (
	"context"
	"fmt"
)

// queryUpdateVehicleLicensePlate writes the already-normalized plate for a
// single vehicle the CALLER OWNS. Ownership is a WHERE-clause predicate, not a
// precondition the caller is trusted to have checked: a mismatched "userId"
// updates zero rows rather than another owner's car. The column is NOT NULL, so
// clearing a plate writes the empty string (never NULL) — which is the
// contract's "no plate set" value on both the column and the wire.
//
// "updatedAt" is bumped so the Prisma side observes the row as changed; no
// telemetry column is touched, so this can never clobber the streaming
// pipeline's writes.
const queryUpdateVehicleLicensePlate = `
UPDATE "Vehicle"
SET "licensePlate" = $1,
    "updatedAt"    = NOW()
WHERE "id" = $2 AND "userId" = $3`

// UpdateLicensePlate sets (or, with an empty plate, clears) the owner-entered
// license plate on a vehicle the given user owns.
//
// The plate MUST already be normalized by the caller — the handler owns
// trim/uppercase/charset/length per rest-api.md §7.14 so the rule lives in one
// place and the store stays a dumb writer. This method does not re-validate.
//
// Returns updated=false (and a nil error) when no row matched, which the
// ownership-scoped WHERE makes indistinguishable between "unknown vehicleId"
// and "vehicle owned by someone else". Callers that need to tell those apart
// (the handler does, to pick 404 vs 403) MUST resolve the row separately first;
// this scoping exists so the write itself is fail-closed regardless.
func (r *VehicleRepo) UpdateLicensePlate(ctx context.Context, userID, vehicleID, plate string) (bool, error) {
	tag, err := r.pool.Exec(ctx, queryUpdateVehicleLicensePlate, plate, vehicleID, userID)
	if err != nil {
		// The plate itself is P1 (data-classification.md §1.3) — never put it in
		// an error string or a log line. Only the P0 ids appear here.
		return false, fmt.Errorf("VehicleRepo.UpdateLicensePlate(user=%s, vehicle=%s): %w", userID, vehicleID, err)
	}
	return tag.RowsAffected() > 0, nil
}
