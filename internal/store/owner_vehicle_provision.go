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

// queryUpsertOwnedVehicle inserts the minimal identity columns for a newly
// linked vehicle, keyed on the unique "teslaVehicleId". On re-link it refreshes
// vin/name/userId only — it never clobbers live telemetry columns (charge, GPS,
// status) written by the streaming pipeline. Columns not listed rely on the
// Prisma-defined DB defaults; if the prod schema has a NOT-NULL column without a
// default the INSERT fails and the caller (a best-effort post-link hook) logs
// and moves on — no orphan row, no link failure. See self-serve-onboarding.md §7.
const queryUpsertOwnedVehicle = `
INSERT INTO "Vehicle" ("id", "userId", "teslaVehicleId", "vin", "name", "updatedAt")
VALUES ($1, $2, $3, $4, $5, NOW())
ON CONFLICT ("teslaVehicleId") DO UPDATE
SET "userId"    = EXCLUDED."userId",
    "vin"       = EXCLUDED."vin",
    "name"      = COALESCE(NULLIF("Vehicle"."name", ''), EXCLUDED."name"),
    "updatedAt" = NOW()`

// UpsertOwnedVehicle seeds (or reconciles) a "Vehicle" identity row for a
// linked owner. Idempotent on "teslaVehicleId".
func (p *OwnerProvisioner) UpsertOwnedVehicle(ctx context.Context, in OwnedVehicleInput) error {
	if strings.TrimSpace(in.UserID) == "" {
		return fmt.Errorf("store.UpsertOwnedVehicle: empty user id")
	}
	if strings.TrimSpace(in.TeslaVehicleID) == "" {
		return fmt.Errorf("store.UpsertOwnedVehicle(user=%s): empty teslaVehicleId", in.UserID)
	}
	name := in.Name
	if strings.TrimSpace(name) == "" {
		name = "Tesla"
	}
	_, err := p.pool.Exec(ctx, queryUpsertOwnedVehicle,
		newProvisionID(), in.UserID, in.TeslaVehicleID, in.VIN, name)
	if err != nil {
		return fmt.Errorf("store.UpsertOwnedVehicle(user=%s, vin=%s): %w", in.UserID, redactVIN(in.VIN), err)
	}
	return nil
}
