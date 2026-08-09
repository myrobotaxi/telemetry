package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/myrobotaxi/telemetry/internal/vin"
)

// OwnedVehicleInput seeds the identity columns of a "Vehicle" row for a
// freshly linked owner (MYR-257). Live values (charge, GPS, status) are NOT
// set here — the streaming telemetry pipeline fills them once the car connects.
type OwnedVehicleInput struct {
	UserID         string
	TeslaVehicleID string
	VIN            string
	Name           string
}

// VehicleUpsertOutcome classifies a vehicle-provision attempt (log-safe, P0).
type VehicleUpsertOutcome string

const (
	// VehicleOwned: the row was inserted or reconciled for this owner.
	VehicleOwned VehicleUpsertOutcome = "owned"
	// VehicleSkippedCrossUser: the teslaVehicleId already belongs to a DIFFERENT
	// user; the row was left untouched (never reassigned) and the caller audits.
	VehicleSkippedCrossUser VehicleUpsertOutcome = "skipped_cross_user"
	// VehicleSkippedTombstoned: the owner deliberately removed this teslaVehicleId
	// (a go_removed_vehicles tombstone exists, MYR-261). The upsert is skipped so a
	// passive Tesla re-link can NOT resurrect a removed car. Cleared only by a
	// deliberate re-add (RemovedVehicleRegistry.ClearTombstone).
	VehicleSkippedTombstoned VehicleUpsertOutcome = "skipped_tombstoned"
)

// queryUpsertOwnedVehicle inserts the minimal identity columns for a newly
// linked vehicle, keyed on the unique "teslaVehicleId". On a same-user conflict
// it refreshes vin/name only (never clobbering live telemetry columns —
// charge/GPS/status — written by the streaming pipeline). The
// `WHERE "Vehicle"."userId" = EXCLUDED."userId"` predicate on the DO UPDATE means
// a conflict against a row owned by a DIFFERENT user updates nothing and reports
// RowsAffected()==0 — the teslaVehicleId is NEVER reassigned across users.
//
// MYR-507: `model` and `year` are no longer seeded with placeholders. That
// comment used to read "the web sync / streaming pipeline fills real values
// later" — and no such writer has ever existed on either side, so for every car
// the GO server provisioned they stayed `”` and `0` permanently. Both are
// `required` wire fields on §7.0 and §7.1, so a rider had no way to name a
// shared car beyond its colour. They are now derived from the VIN
// (`internal/vin`) and passed in as binds, which is possible precisely because
// the VIN needs no token, no awake car and no network call: a car is correctly
// identified from the instant it is linked, rather than waiting on a
// connectivity edge it may never produce.
//
// The ON CONFLICT arm BACKFILLS both — `NULLIF`-guarded exactly like `name`
// above, so a re-link fills an empty column but never overwrites a populated
// one. The Prisma web-link flow writes a richer model for some rows ("Model 3
// Performance") than position 4 of a VIN can encode, and a re-link must not
// downgrade it. (`VehicleRepo.FillVehicleIdentity` applies the same rule on the
// refresh path for cars already in the table.)
//
// `color` and `licensePlate` keep their empty placeholders: neither is
// derivable from the VIN. `color` is filled by the MYR-320 Tesla read;
// `licensePlate` only ever comes from the owner typing it (§7.14).
//
// `xmax = 0` is Postgres's "row was inserted (not updated) by this statement"
// test, used to distinguish an insert from a same-user reconcile.
const queryUpsertOwnedVehicle = `
INSERT INTO "Vehicle" ("id", "userId", "teslaVehicleId", "vin", "name",
                       "model", "year", "color", "licensePlate", "updatedAt")
VALUES ($1, $2, $3, $4, $5, $6, $7, '', '', NOW())
ON CONFLICT ("teslaVehicleId") DO UPDATE
SET "vin"       = EXCLUDED."vin",
    "name"      = COALESCE(NULLIF("Vehicle"."name", ''), EXCLUDED."name"),
    "model"     = COALESCE(NULLIF("Vehicle"."model", ''), EXCLUDED."model"),
    "year"      = COALESCE(NULLIF("Vehicle"."year", 0), EXCLUDED."year"),
    "updatedAt" = NOW()
WHERE "Vehicle"."userId" = EXCLUDED."userId"`

// UpsertOwnedVehicle seeds (or reconciles) a "Vehicle" identity row for a linked
// owner. Idempotent on "teslaVehicleId". Returns VehicleSkippedTombstoned (and no
// error) when the owner deliberately removed this teslaVehicleId (MYR-261 — the
// tombstone gate that stops a passive re-link from resurrecting a removed car),
// and VehicleSkippedCrossUser when the teslaVehicleId is already owned by a
// different user — the row is never reassigned; the caller emits an audit line.
func (p *OwnerProvisioner) UpsertOwnedVehicle(ctx context.Context, in OwnedVehicleInput) (VehicleUpsertOutcome, error) {
	if strings.TrimSpace(in.UserID) == "" {
		return "", fmt.Errorf("store.UpsertOwnedVehicle: empty user id")
	}
	if strings.TrimSpace(in.TeslaVehicleID) == "" {
		return "", fmt.Errorf("store.UpsertOwnedVehicle(user=%s): empty teslaVehicleId", in.UserID)
	}
	// Removed-vehicle tombstone gate (MYR-261): if the owner deliberately removed
	// this teslaVehicleId, SKIP the upsert. This is the fix for the reappearance
	// bug — the teardown leaves Tesla access intact, so without this gate the
	// best-effort AfterLink sync would re-INSERT the still-owned VIN. The check
	// lives here so it covers EVERY re-add sync route through UpsertOwnedVehicle,
	// not just AfterLink. A deliberate re-add clears the tombstone first.
	tombstoned, err := isVehicleTombstoned(ctx, p.pool, in.UserID, in.TeslaVehicleID)
	if err != nil {
		return "", fmt.Errorf("store.UpsertOwnedVehicle(user=%s, vin=%s): %w", in.UserID, redactVIN(in.VIN), err)
	}
	if tombstoned {
		return VehicleSkippedTombstoned, nil
	}

	name := in.Name
	if strings.TrimSpace(name) == "" {
		name = "Tesla"
	}
	// MYR-507: the identity a VIN can be read for without asking Tesla anything.
	// Either may be the zero value for an unrecognised code, which the INSERT
	// stores as the same placeholder this statement used to hard-code and which
	// the refresh path (FillVehicleIdentity) will retry against later.
	tag, err := p.pool.Exec(ctx, queryUpsertOwnedVehicle,
		newProvisionID(), in.UserID, in.TeslaVehicleID, in.VIN, name,
		vin.Model(in.VIN), vin.ModelYear(in.VIN))
	if err != nil {
		return "", fmt.Errorf("store.UpsertOwnedVehicle(user=%s, vin=%s): %w", in.UserID, redactVIN(in.VIN), err)
	}
	// A same-teslaVehicleId row owned by another user fails the DO UPDATE WHERE
	// predicate → zero rows affected → cross-user skip (never a reassignment).
	if tag.RowsAffected() == 0 {
		return VehicleSkippedCrossUser, nil
	}
	return VehicleOwned, nil
}
