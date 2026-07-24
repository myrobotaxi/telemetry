package store

import (
	"context"
	"fmt"
	"strings"
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
)

// queryUpsertOwnedVehicle inserts the minimal identity columns for a newly
// linked vehicle, keyed on the unique "teslaVehicleId". On a same-user conflict
// it refreshes vin/name only (never clobbering live telemetry columns —
// charge/GPS/status — written by the streaming pipeline). The
// `WHERE "Vehicle"."userId" = EXCLUDED."userId"` predicate on the DO UPDATE means
// a conflict against a row owned by a DIFFERENT user updates nothing and reports
// RowsAffected()==0 — the teslaVehicleId is NEVER reassigned across users.
//
// The four NOT-NULL identity columns without Prisma defaults (`model`, `year`,
// `color`, `licensePlate`) are seeded with empty placeholders so the INSERT
// succeeds against the prod schema; the web sync / streaming pipeline fills real
// values later. `xmax = 0` is Postgres's "row was inserted (not updated) by this
// statement" test, used to distinguish an insert from a same-user reconcile.
const queryUpsertOwnedVehicle = `
INSERT INTO "Vehicle" ("id", "userId", "teslaVehicleId", "vin", "name",
                       "model", "year", "color", "licensePlate", "updatedAt")
VALUES ($1, $2, $3, $4, $5, '', 0, '', '', NOW())
ON CONFLICT ("teslaVehicleId") DO UPDATE
SET "vin"       = EXCLUDED."vin",
    "name"      = COALESCE(NULLIF("Vehicle"."name", ''), EXCLUDED."name"),
    "updatedAt" = NOW()
WHERE "Vehicle"."userId" = EXCLUDED."userId"`

// UpsertOwnedVehicle seeds (or reconciles) a "Vehicle" identity row for a linked
// owner. Idempotent on "teslaVehicleId". Returns VehicleSkippedCrossUser (and no
// error) when the teslaVehicleId is already owned by a different user — the row
// is never reassigned; the caller emits an audit line.
func (p *OwnerProvisioner) UpsertOwnedVehicle(ctx context.Context, in OwnedVehicleInput) (VehicleUpsertOutcome, error) {
	if strings.TrimSpace(in.UserID) == "" {
		return "", fmt.Errorf("store.UpsertOwnedVehicle: empty user id")
	}
	if strings.TrimSpace(in.TeslaVehicleID) == "" {
		return "", fmt.Errorf("store.UpsertOwnedVehicle(user=%s): empty teslaVehicleId", in.UserID)
	}
	name := in.Name
	if strings.TrimSpace(name) == "" {
		name = "Tesla"
	}
	tag, err := p.pool.Exec(ctx, queryUpsertOwnedVehicle,
		newProvisionID(), in.UserID, in.TeslaVehicleID, in.VIN, name)
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
