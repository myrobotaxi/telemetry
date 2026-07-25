package store_test

import (
	"context"
	"log/slog"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

func newTestRegistry() *store.RemovedVehicleRegistry {
	return store.NewRemovedVehicleRegistry(testPool,
		slog.New(slog.NewTextHandler(discardWriter{}, nil)))
}

// seedTombstone inserts a removed-vehicle tombstone directly (simulating a
// prior teardown) so the sync-path skip can be tested in isolation.
func seedTombstone(t *testing.T, userID, teslaVehicleID, vin string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO go_removed_vehicles (user_id, tesla_vehicle_id, vin, removed_at)
		 VALUES ($1,$2,NULLIF($3,''),NOW())
		 ON CONFLICT (user_id, tesla_vehicle_id) DO UPDATE SET removed_at = NOW()`,
		userID, teslaVehicleID, vin); err != nil {
		t.Fatalf("seed tombstone: %v", err)
	}
}

// TestUpsertOwnedVehicle_SkipsTombstonedVIN is the core reappearance guard: a
// tombstoned (user, teslaVehicleId) must be skipped by the provisioning sync
// path and no Vehicle row must be inserted.
func TestUpsertOwnedVehicle_SkipsTombstonedVIN(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ensureTeardownSchema(t)
	cleanTeardownTables(t)

	const uid, tvid, vin = "crmvowner001", "vid-removed", "5YJ3E1EA7KF000201"
	seedOwnerUser(t, uid, "", "")
	seedTombstone(t, uid, tvid, vin)

	out, err := newTestProvisioner(t).UpsertOwnedVehicle(context.Background(), store.OwnedVehicleInput{
		UserID: uid, TeslaVehicleID: tvid, VIN: vin, Name: "Should not appear",
	})
	if err != nil {
		t.Fatalf("UpsertOwnedVehicle: %v", err)
	}
	if out != store.VehicleSkippedTombstoned {
		t.Errorf("outcome = %q, want skipped_tombstoned", out)
	}
	if n := countRows(t, `"Vehicle"`, `"teslaVehicleId"`, tvid); n != 0 {
		t.Errorf("Vehicle rows = %d, want 0 (tombstoned, must not be inserted)", n)
	}
}

// TestUpsertOwnedVehicle_NonTombstonedUpsertsNormally confirms a VIN with no
// tombstone still provisions normally (the fix must not break the happy path).
func TestUpsertOwnedVehicle_NonTombstonedUpsertsNormally(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ensureTeardownSchema(t)
	cleanTeardownTables(t)

	const uid, tvid, vin = "crmvowner002", "vid-fresh", "5YJ3E1EA7KF000202"
	seedOwnerUser(t, uid, "", "")

	out, err := newTestProvisioner(t).UpsertOwnedVehicle(context.Background(), store.OwnedVehicleInput{
		UserID: uid, TeslaVehicleID: tvid, VIN: vin, Name: "Fresh",
	})
	if err != nil {
		t.Fatalf("UpsertOwnedVehicle: %v", err)
	}
	if out != store.VehicleOwned {
		t.Errorf("outcome = %q, want owned", out)
	}
	if n := countRows(t, `"Vehicle"`, `"teslaVehicleId"`, tvid); n != 1 {
		t.Errorf("Vehicle rows = %d, want 1 (non-tombstoned upserts)", n)
	}
}

// TestClearTombstone_ThenReadd exercises the deliberate re-add: clearing the
// tombstone lets the next sync provision the car again, and clearing is audited
// and idempotent.
func TestClearTombstone_ThenReadd(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ensureTeardownSchema(t) // AuditLog + go_removed_vehicles + Vehicle
	cleanTeardownTables(t)

	const uid, tvid, vin = "crmvowner003", "vid-readd", "5YJ3E1EA7KF000203"
	seedOwnerUser(t, uid, "", "")
	seedTombstone(t, uid, tvid, vin)
	prov := newTestProvisioner(t)
	reg := newTestRegistry()
	ctx := context.Background()

	// While tombstoned, sync skips.
	out, err := prov.UpsertOwnedVehicle(ctx, store.OwnedVehicleInput{UserID: uid, TeslaVehicleID: tvid, VIN: vin})
	if err != nil {
		t.Fatalf("upsert (tombstoned): %v", err)
	}
	if out != store.VehicleSkippedTombstoned {
		t.Fatalf("outcome = %q, want skipped_tombstoned", out)
	}

	// Deliberate re-add: clear the tombstone.
	cleared, err := reg.ClearTombstone(ctx, uid, tvid)
	if err != nil {
		t.Fatalf("ClearTombstone: %v", err)
	}
	if !cleared {
		t.Fatalf("ClearTombstone returned false, want true (a tombstone existed)")
	}
	if tombstoneExists(t, uid, tvid) {
		t.Errorf("tombstone still present after clear")
	}

	// Now the sync provisions the car again.
	out, err = prov.UpsertOwnedVehicle(ctx, store.OwnedVehicleInput{UserID: uid, TeslaVehicleID: tvid, VIN: vin, Name: "Back"})
	if err != nil {
		t.Fatalf("upsert (after clear): %v", err)
	}
	if out != store.VehicleOwned {
		t.Errorf("outcome = %q, want owned (car returns after clear)", out)
	}
	if n := countRows(t, `"Vehicle"`, `"teslaVehicleId"`, tvid); n != 1 {
		t.Errorf("Vehicle rows = %d, want 1 (car returned)", n)
	}

	// vehicle_readd_allowed audit written, targetId = teslaVehicleId, P0.
	assertReaddAudit(t, uid, tvid)

	// Idempotent: clearing an absent tombstone is a clean no-op (no new audit).
	cleared, err = reg.ClearTombstone(ctx, uid, tvid)
	if err != nil {
		t.Fatalf("ClearTombstone (second): %v", err)
	}
	if cleared {
		t.Errorf("second ClearTombstone returned true, want false (already gone)")
	}
	if n := countRows(t, `"AuditLog"`, `"targetId"`, tvid); n != 1 {
		t.Errorf("vehicle_readd_allowed audit rows = %d, want exactly 1 (no dup on no-op)", n)
	}
}

func assertReaddAudit(t *testing.T, userID, teslaVehicleID string) {
	t.Helper()
	var action, targetType, initiator string
	if err := testPool.QueryRow(context.Background(),
		`SELECT "action","targetType","initiator" FROM "AuditLog" WHERE "userId"=$1 AND "targetId"=$2`,
		userID, teslaVehicleID).Scan(&action, &targetType, &initiator); err != nil {
		t.Fatalf("read readd audit: %v", err)
	}
	if action != "vehicle_readd_allowed" || targetType != "vehicle" || initiator != "user" {
		t.Errorf("audit = (action=%q, targetType=%q, initiator=%q), want (vehicle_readd_allowed, vehicle, user)",
			action, targetType, initiator)
	}
}
