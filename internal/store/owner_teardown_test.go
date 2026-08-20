package store_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// teardownSchemaSQL adds the pieces the OwnerTeardown exercises that the shared
// TestMain / owner-provision fixtures don't provide: the AuditLog table it
// INSERTs into, a TripStop table that cascades with the Vehicle, ON DELETE
// CASCADE on the Drive/TripStop FKs (the prod Prisma schema declares these; the
// slim shared fixture omits them), and the Next.js-owned `vehicle_deleted`
// AFTER DELETE NOTIFY trigger so the stream-close path can be asserted.
const teardownSchemaSQL = `
CREATE TABLE IF NOT EXISTS "AuditLog" (
    "id"          TEXT PRIMARY KEY,
    "userId"      TEXT NOT NULL,
    "timestamp"   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    "action"      TEXT NOT NULL,
    "targetType"  TEXT NOT NULL,
    "targetId"    TEXT NOT NULL,
    "initiator"   TEXT NOT NULL,
    "metadata"    JSONB DEFAULT '{}',
    "createdAt"   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS "TripStop" (
    "id"        TEXT PRIMARY KEY,
    "vehicleId" TEXT NOT NULL REFERENCES "Vehicle"("id") ON DELETE CASCADE,
    "name"      TEXT NOT NULL DEFAULT ''
);
-- Go-owned ride-request table (migration 0002). Slim mirror: vehicle_id has NO
-- FK to "Vehicle" (CG-DL-9), so the teardown must delete these rows explicitly.
CREATE TABLE IF NOT EXISTS go_ride_requests (
    id              TEXT PRIMARY KEY,
    rider_id        TEXT NOT NULL,
    owner_id        TEXT NOT NULL,
    vehicle_id      TEXT NOT NULL,
    pickup_lat_enc  TEXT NOT NULL,
    pickup_lng_enc  TEXT NOT NULL,
    pickup_label    TEXT NOT NULL,
    dropoff_lat_enc TEXT NOT NULL,
    dropoff_lng_enc TEXT NOT NULL,
    dropoff_label   TEXT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'requested',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- Re-point the shared Drive FK to cascade on Vehicle delete (prod parity).
ALTER TABLE "Drive" DROP CONSTRAINT IF EXISTS "Drive_vehicleId_fkey";
ALTER TABLE "Drive" ADD CONSTRAINT "Drive_vehicleId_fkey"
    FOREIGN KEY ("vehicleId") REFERENCES "Vehicle"("id") ON DELETE CASCADE;
-- vehicle_deleted NOTIFY trigger (mirrors the Next.js-owned trigger the
-- telemetry server's notify_listener consumes).
CREATE OR REPLACE FUNCTION notify_vehicle_deleted() RETURNS TRIGGER AS $$
BEGIN
    PERFORM pg_notify('vehicle_deleted', json_build_object(
        'vehicleId', OLD."id",
        'userId',    OLD."userId",
        'vin',       COALESCE(OLD."vin", '')
    )::text);
    RETURN OLD;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_vehicle_deleted ON "Vehicle";
CREATE TRIGGER trg_vehicle_deleted AFTER DELETE ON "Vehicle"
    FOR EACH ROW EXECUTE FUNCTION notify_vehicle_deleted();
`

func ensureTeardownSchema(t *testing.T) {
	t.Helper()
	ensureOwnerSchema(t) // Settings + Account
	// The Go-owned namespace, from the migrations themselves rather than from a
	// hand-copy: the teardown transaction touches go_ride_requests (0002),
	// go_vehicle_shares (0020), go_removed_vehicles (0006) and, since MYR-592,
	// go_vehicle_telemetry_suspensions (0044) and go_fleet_config_attempts
	// (0031) — none of which the shared TestMain builds.
	//
	// Added with MYR-592, and it closes a PRE-EXISTING fragility rather than one
	// this feature introduced: these tests already passed only because another
	// file in the package (`migration_00xx_test.go`) happens to sort earlier and
	// applies the migrations as a side effect. Run `-run TestOwnerTeardown` alone
	// before this line and the suite fails on a missing go_vehicle_shares. A
	// passing test for the wrong reason is one edit away from a failing one for
	// no reason. RunMigrations is idempotent, so calling it per test is free.
	mustApplyGoMigrations(t)
	if _, err := testPool.Exec(context.Background(), teardownSchemaSQL); err != nil {
		t.Fatalf("apply teardown schema: %v", err)
	}
}

func cleanTeardownTables(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	// NOTE: "AuditLog" is intentionally omitted — it is append-only (the
	// mask-audit integration test installs the prevent-mutation triggers, so a
	// DELETE would raise SQLSTATE P0001). Every audit assertion below filters
	// by a per-test-unique userId/targetId, so residual rows never collide.
	for _, tbl := range []string{`go_ride_requests`, `go_removed_vehicles`, `go_vehicle_telemetry_suspensions`,
		`go_fleet_config_attempts`, `"TripStop"`, `"Drive"`, `"Vehicle"`, `"Account"`, `"Settings"`, `"User"`} {
		if _, err := testPool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}
}

func newTestTeardown() *store.OwnerTeardown {
	return store.NewOwnerTeardown(testPool, slog.New(slog.NewTextHandler(discardWriter{}, nil)))
}

func seedTeardownVehicle(t *testing.T, id, userID, vin string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO "Vehicle" ("id","userId","vin","name","status") VALUES ($1,$2,$3,'Test','parked')`,
		id, userID, vin); err != nil {
		t.Fatalf("seed vehicle %s: %v", id, err)
	}
}

func seedTeardownVehicleWithTesla(t *testing.T, id, userID, vin, teslaVehicleID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO "Vehicle" ("id","userId","vin","name","status","teslaVehicleId") VALUES ($1,$2,$3,'Test','parked',$4)`,
		id, userID, vin, teslaVehicleID); err != nil {
		t.Fatalf("seed vehicle %s: %v", id, err)
	}
}

func tombstoneExists(t *testing.T, userID, teslaVehicleID string) bool {
	t.Helper()
	var ok bool
	if err := testPool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM go_removed_vehicles WHERE user_id=$1 AND tesla_vehicle_id=$2)`,
		userID, teslaVehicleID).Scan(&ok); err != nil {
		t.Fatalf("tombstone exists check: %v", err)
	}
	return ok
}

func seedTeardownDrive(t *testing.T, id, vehicleID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO "Drive" ("id","vehicleId","date","startTime") VALUES ($1,$2,'2026-07-24','10:00')`,
		id, vehicleID); err != nil {
		t.Fatalf("seed drive %s: %v", id, err)
	}
}

func seedTeardownRideRequest(t *testing.T, id, ownerID, vehicleID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO go_ride_requests
		 (id, rider_id, owner_id, vehicle_id,
		  pickup_lat_enc, pickup_lng_enc, pickup_label,
		  dropoff_lat_enc, dropoff_lng_enc, dropoff_label, status)
		 VALUES ($1,$2,$3,$4,'enc','enc','Pickup','enc','enc','Dropoff','completed')`,
		id, ownerID, ownerID, vehicleID); err != nil {
		t.Fatalf("seed ride request %s: %v", id, err)
	}
}

func seedTeslaAccount(t *testing.T, userID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO "Account" ("id","userId","provider","providerAccountId","access_token")
		 VALUES ($1,$2,'tesla',$3,'tok')`,
		"acct_"+userID, userID, "sub_"+userID); err != nil {
		t.Fatalf("seed account: %v", err)
	}
}

func seedTeslaLinkedSettings(t *testing.T, userID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO "Settings" ("id","userId","teslaLinked","virtualKeyPaired","keyPairingReminderCount")
		 VALUES ($1,$2,TRUE,TRUE,2)`,
		"set_"+userID, userID); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
}

func TestOwnerTeardown_RemoveVehicle_HappyPathLastVehicle(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ensureTeardownSchema(t)
	cleanTeardownTables(t)

	const userID, vehicleID, vin = "user_last", "veh_last", "5YJ3E1EA1PF000010"
	seedOwnerUser(t, userID, "Owner", userID+"@example.com")
	seedTeardownVehicle(t, vehicleID, userID, vin)
	seedTeardownDrive(t, "drive_1", vehicleID)
	seedTeardownDrive(t, "drive_2", vehicleID)
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO "TripStop" ("id","vehicleId") VALUES ('stop_1',$1)`, vehicleID); err != nil {
		t.Fatalf("seed tripstop: %v", err)
	}
	seedTeslaAccount(t, userID)
	seedTeslaLinkedSettings(t, userID)
	seedTeardownRideRequest(t, "rr_1", userID, vehicleID)
	// A ride request for a DIFFERENT vehicle must survive.
	seedTeardownVehicle(t, "veh_other_owner", "user_other_rr", "5YJ3E1EA1PF000099")
	seedOwnerUser(t, "user_other_rr", "Other", "user_other_rr@example.com")
	seedTeardownRideRequest(t, "rr_keep", "user_other_rr", "veh_other_owner")

	res, err := newTestTeardown().RemoveVehicle(context.Background(), userID, vehicleID)
	if err != nil {
		t.Fatalf("RemoveVehicle: %v", err)
	}

	if !res.Removed || res.AlreadyGone {
		t.Errorf("result = %+v, want Removed && !AlreadyGone", res)
	}
	if !res.WasLastVehicle || !res.TeslaTokensCleared {
		t.Errorf("result = %+v, want WasLastVehicle && TeslaTokensCleared", res)
	}
	if res.DriveCount != 2 {
		t.Errorf("DriveCount = %d, want 2", res.DriveCount)
	}

	// Vehicle + cascade gone.
	if n := countRows(t, `"Vehicle"`, `"id"`, vehicleID); n != 0 {
		t.Errorf("Vehicle rows = %d, want 0", n)
	}
	if n := countRows(t, `"Drive"`, `"vehicleId"`, vehicleID); n != 0 {
		t.Errorf("Drive rows = %d, want 0 (cascade)", n)
	}
	if n := countRows(t, `"TripStop"`, `"vehicleId"`, vehicleID); n != 0 {
		t.Errorf("TripStop rows = %d, want 0 (cascade)", n)
	}
	// Last-vehicle: Account cleared + Settings reset.
	if n := countRows(t, `"Account"`, `"userId"`, userID); n != 0 {
		t.Errorf("Account rows = %d, want 0 (last vehicle)", n)
	}
	assertSettingsReset(t, userID)

	// Ride-request rows (P1 GPS + PII) for this vehicle deleted; another
	// vehicle's ride request untouched.
	if n := countRows(t, "go_ride_requests", "vehicle_id", vehicleID); n != 0 {
		t.Errorf("go_ride_requests for removed vehicle = %d, want 0", n)
	}
	if n := countRows(t, "go_ride_requests", "vehicle_id", "veh_other_owner"); n != 1 {
		t.Errorf("go_ride_requests for other vehicle = %d, want 1 (untouched)", n)
	}

	// User row is NEVER touched.
	if n := countRows(t, `"User"`, `"id"`, userID); n != 1 {
		t.Errorf("User rows = %d, want 1 (retained)", n)
	}

	// Exactly one vehicle_deleted audit row, P0 metadata only.
	assertVehicleDeletedAudit(t, userID, vehicleID, 2, true)
}

func TestOwnerTeardown_NonLastVehicle_KeepsAccount(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ensureTeardownSchema(t)
	cleanTeardownTables(t)

	const userID = "user_multi"
	seedOwnerUser(t, userID, "Owner", userID+"@example.com")
	seedTeardownVehicle(t, "veh_a", userID, "5YJ3E1EA1PF000020")
	seedTeardownVehicle(t, "veh_b", userID, "5YJ3E1EA1PF000021")
	seedTeslaAccount(t, userID)
	seedTeslaLinkedSettings(t, userID)

	res, err := newTestTeardown().RemoveVehicle(context.Background(), userID, "veh_a")
	if err != nil {
		t.Fatalf("RemoveVehicle: %v", err)
	}
	if res.WasLastVehicle || res.TeslaTokensCleared {
		t.Errorf("result = %+v, want !WasLastVehicle && !TeslaTokensCleared", res)
	}

	if n := countRows(t, `"Vehicle"`, `"id"`, "veh_b"); n != 1 {
		t.Errorf("other vehicle rows = %d, want 1 (kept)", n)
	}
	if n := countRows(t, `"Account"`, `"userId"`, userID); n != 1 {
		t.Errorf("Account rows = %d, want 1 (not last vehicle)", n)
	}
	// Settings link flags untouched.
	linked, paired := getSettingsFlags(t, userID)
	if !linked || !paired {
		t.Errorf("Settings flags = (linked=%v, paired=%v), want both true", linked, paired)
	}
	assertVehicleDeletedAudit(t, userID, "veh_a", 0, false)
}

func TestOwnerTeardown_Idempotent(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ensureTeardownSchema(t)
	cleanTeardownTables(t)

	const userID, vehicleID = "user_idem", "veh_idem"
	seedOwnerUser(t, userID, "Owner", userID+"@example.com")
	seedTeardownVehicle(t, vehicleID, userID, "5YJ3E1EA1PF000030")

	td := newTestTeardown()
	if _, err := td.RemoveVehicle(context.Background(), userID, vehicleID); err != nil {
		t.Fatalf("first remove: %v", err)
	}
	// Re-run: clean no-op success, no error, no second audit row.
	res, err := td.RemoveVehicle(context.Background(), userID, vehicleID)
	if err != nil {
		t.Fatalf("second remove: %v", err)
	}
	if !res.AlreadyGone || res.Removed {
		t.Errorf("result = %+v, want AlreadyGone && !Removed", res)
	}
	if n := countRows(t, `"AuditLog"`, `"targetId"`, vehicleID); n != 1 {
		t.Errorf("audit rows = %d, want exactly 1 (no duplicate on re-run)", n)
	}
}

func TestOwnerTeardown_CrossUserRejected(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ensureTeardownSchema(t)
	cleanTeardownTables(t)

	const ownerID, attackerID, vehicleID = "user_owner", "user_attacker", "veh_owned"
	seedOwnerUser(t, ownerID, "Owner", ownerID+"@example.com")
	seedOwnerUser(t, attackerID, "Attacker", attackerID+"@example.com")
	seedTeardownVehicle(t, vehicleID, ownerID, "5YJ3E1EA1PF000040")

	// Attacker attempts to remove the owner's vehicle.
	res, err := newTestTeardown().RemoveVehicle(context.Background(), attackerID, vehicleID)
	if err != nil {
		t.Fatalf("RemoveVehicle: %v", err)
	}
	if !res.AlreadyGone || res.Removed {
		t.Errorf("cross-user result = %+v, want AlreadyGone && !Removed (no deletion)", res)
	}
	// The owner's vehicle MUST still exist — owner scope enforced at SQL level.
	if n := countRows(t, `"Vehicle"`, `"id"`, vehicleID); n != 1 {
		t.Errorf("owner's vehicle rows = %d, want 1 (untouched)", n)
	}
	// No audit row written for a rejected cross-user attempt.
	if n := countRows(t, `"AuditLog"`, `"targetId"`, vehicleID); n != 0 {
		t.Errorf("audit rows = %d, want 0 (no delete → no audit)", n)
	}
}

func TestOwnerTeardown_FiresVehicleDeletedNotify(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ensureTeardownSchema(t)
	cleanTeardownTables(t)

	const userID, vehicleID, vin = "user_notify", "veh_notify", "5YJ3E1EA1PF000050"
	seedOwnerUser(t, userID, "Owner", userID+"@example.com")
	seedTeardownVehicle(t, vehicleID, userID, vin)

	// Dedicated LISTEN connection (LISTEN is per-connection state).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, testConnStr)
	if err != nil {
		t.Fatalf("listen connect: %v", err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, "LISTEN "+store.VehicleDeletedChannel); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	if _, err := newTestTeardown().RemoveVehicle(context.Background(), userID, vehicleID); err != nil {
		t.Fatalf("RemoveVehicle: %v", err)
	}

	notif, err := conn.WaitForNotification(ctx)
	if err != nil {
		t.Fatalf("WaitForNotification: %v", err)
	}
	if notif.Channel != store.VehicleDeletedChannel {
		t.Errorf("channel = %q, want %q", notif.Channel, store.VehicleDeletedChannel)
	}
	var payload struct {
		VehicleID string `json:"vehicleId"`
		UserID    string `json:"userId"`
		VIN       string `json:"vin"`
	}
	if err := json.Unmarshal([]byte(notif.Payload), &payload); err != nil {
		t.Fatalf("payload decode: %v (raw %q)", err, notif.Payload)
	}
	if payload.VehicleID != vehicleID || payload.UserID != userID || payload.VIN != vin {
		t.Errorf("payload = %+v, want vehicleId=%s userId=%s vin=%s", payload, vehicleID, userID, vin)
	}
}

// --- assertion helpers -----------------------------------------------------

func assertSettingsReset(t *testing.T, userID string) {
	t.Helper()
	linked, paired := getSettingsFlags(t, userID)
	if linked || paired {
		t.Errorf("Settings flags = (linked=%v, paired=%v), want both false", linked, paired)
	}
	var reminder int
	if err := testPool.QueryRow(context.Background(),
		`SELECT "keyPairingReminderCount" FROM "Settings" WHERE "userId" = $1`, userID).Scan(&reminder); err != nil {
		t.Fatalf("read reminder count: %v", err)
	}
	if reminder != 0 {
		t.Errorf("keyPairingReminderCount = %d, want 0", reminder)
	}
}

func getSettingsFlags(t *testing.T, userID string) (linked, paired bool) {
	t.Helper()
	if err := testPool.QueryRow(context.Background(),
		`SELECT "teslaLinked", "virtualKeyPaired" FROM "Settings" WHERE "userId" = $1`, userID).
		Scan(&linked, &paired); err != nil {
		t.Fatalf("read settings flags: %v", err)
	}
	return linked, paired
}

func assertVehicleDeletedAudit(t *testing.T, userID, vehicleID string, wantDriveCount int, wantLast bool) {
	t.Helper()
	var (
		action, targetType, targetID, initiator string
		metadata                                []byte
	)
	err := testPool.QueryRow(context.Background(),
		`SELECT "action","targetType","targetId","initiator","metadata"
		 FROM "AuditLog" WHERE "userId" = $1 AND "targetId" = $2`, userID, vehicleID).
		Scan(&action, &targetType, &targetID, &initiator, &metadata)
	if err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if action != "vehicle_deleted" || targetType != "vehicle" || initiator != "user" {
		t.Errorf("audit = (action=%q, targetType=%q, initiator=%q), want (vehicle_deleted, vehicle, user)", action, targetType, initiator)
	}
	var meta struct {
		DriveCount     int  `json:"driveCount"`
		WasLastVehicle bool `json:"wasLastVehicle"`
	}
	if err := json.Unmarshal(metadata, &meta); err != nil {
		t.Fatalf("audit metadata decode: %v", err)
	}
	if meta.DriveCount != wantDriveCount || meta.WasLastVehicle != wantLast {
		t.Errorf("audit metadata = %+v, want driveCount=%d wasLastVehicle=%v", meta, wantDriveCount, wantLast)
	}
}

// TestOwnerTeardown_WritesTombstone verifies the teardown writes a
// removed-vehicle tombstone in the SAME transaction as the delete (MYR-261),
// records it on the vehicle_deleted audit metadata, and skips the tombstone for
// a car with no Tesla vehicle id.
func TestOwnerTeardown_WritesTombstone(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ensureTeardownSchema(t)
	cleanTeardownTables(t)

	const userID, vehicleID, vin, tvid = "user_tomb", "veh_tomb", "5YJ3E1EA1PF000210", "tesla-veh-210"
	seedOwnerUser(t, userID, "Owner", userID+"@example.com")
	seedTeardownVehicleWithTesla(t, vehicleID, userID, vin, tvid)

	res, err := newTestTeardown().RemoveVehicle(context.Background(), userID, vehicleID)
	if err != nil {
		t.Fatalf("RemoveVehicle: %v", err)
	}
	if !res.Removed || !res.Tombstoned {
		t.Errorf("result = %+v, want Removed && Tombstoned", res)
	}
	if !tombstoneExists(t, userID, tvid) {
		t.Errorf("no tombstone written for (%s, %s)", userID, tvid)
	}
	// Tombstone create recorded on the vehicle_deleted audit metadata.
	var metadata []byte
	if err := testPool.QueryRow(context.Background(),
		`SELECT "metadata" FROM "AuditLog" WHERE "userId"=$1 AND "targetId"=$2`, userID, vehicleID).
		Scan(&metadata); err != nil {
		t.Fatalf("read audit metadata: %v", err)
	}
	if !containsTombstonedTrue(metadata) {
		t.Errorf("audit metadata = %s, want tombstoned:true", metadata)
	}
}

// TestOwnerTeardown_NoTeslaId_NoTombstone confirms a car with no Tesla vehicle
// id is removed but NOT tombstoned (nothing a Fleet-API sync could resurrect).
func TestOwnerTeardown_NoTeslaId_NoTombstone(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ensureTeardownSchema(t)
	cleanTeardownTables(t)

	const userID, vehicleID, vin = "user_notesla", "veh_notesla", "5YJ3E1EA1PF000211"
	seedOwnerUser(t, userID, "Owner", userID+"@example.com")
	seedTeardownVehicle(t, vehicleID, userID, vin) // no teslaVehicleId

	res, err := newTestTeardown().RemoveVehicle(context.Background(), userID, vehicleID)
	if err != nil {
		t.Fatalf("RemoveVehicle: %v", err)
	}
	if !res.Removed || res.Tombstoned {
		t.Errorf("result = %+v, want Removed && !Tombstoned", res)
	}
	var n int
	if err := testPool.QueryRow(context.Background(),
		`SELECT count(*) FROM go_removed_vehicles WHERE user_id=$1`, userID).Scan(&n); err != nil {
		t.Fatalf("count tombstones: %v", err)
	}
	if n != 0 {
		t.Errorf("tombstone rows = %d, want 0 (no Tesla id → nothing to resurrect)", n)
	}
}

// TestOwnerTeardown_RemoveThenSync_StaysGone is the exact reappearance repro:
// remove a car, then run the Tesla-link sync (UpsertOwnedVehicle) for the same
// still-Tesla-owned id — it must stay gone.
func TestOwnerTeardown_RemoveThenSync_StaysGone(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ensureTeardownSchema(t)
	cleanTeardownTables(t)

	const userID, vehicleID, vin, tvid = "user_repro", "veh_repro", "5YJ3E1EA1PF000212", "tesla-veh-212"
	seedOwnerUser(t, userID, "Owner", userID+"@example.com")
	// A second vehicle so the removed one is NOT the last (tokens stay intact —
	// the exact condition under which the resurrection bug fired).
	seedTeardownVehicleWithTesla(t, vehicleID, userID, vin, tvid)
	seedTeardownVehicleWithTesla(t, "veh_keep", userID, "5YJ3E1EA1PF000213", "tesla-veh-213")

	if _, err := newTestTeardown().RemoveVehicle(context.Background(), userID, vehicleID); err != nil {
		t.Fatalf("RemoveVehicle: %v", err)
	}

	// Simulate the post-link Fleet-API sync re-offering the still-owned id.
	out, err := newTestProvisioner(t).UpsertOwnedVehicle(context.Background(), store.OwnedVehicleInput{
		UserID: userID, TeslaVehicleID: tvid, VIN: vin, Name: "Resurrected?",
	})
	if err != nil {
		t.Fatalf("UpsertOwnedVehicle (sync): %v", err)
	}
	if out != store.VehicleSkippedTombstoned {
		t.Errorf("sync outcome = %q, want skipped_tombstoned (car must stay gone)", out)
	}
	if n := countRows(t, `"Vehicle"`, `"teslaVehicleId"`, tvid); n != 0 {
		t.Errorf("removed vehicle rows = %d, want 0 (must not reappear)", n)
	}
}

func containsTombstonedTrue(metadata []byte) bool {
	var m struct {
		Tombstoned bool `json:"tombstoned"`
	}
	if err := json.Unmarshal(metadata, &m); err != nil {
		return false
	}
	return m.Tombstoned
}

// TestOwnerTeardown_ClearsTelemetrySuspension pins the UNLINK COMPLETELY half of
// the two offers the MYR-592 disconnect notice makes (rest-api.md §7.0): it has
// to actually finish.
//
// No FK reaches this table (CG-DL-9), so nothing cascades — the DELETE is
// explicit or it does not happen. A surviving row would be a suspension stamp
// keyed to a car that no longer exists: inert today, because every reader joins
// from a live vehicle, and a landmine the moment a cuid is reused or somebody
// reads the table directly.
func TestOwnerTeardown_ClearsTelemetrySuspension(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ensureTeardownSchema(t)
	cleanTeardownTables(t)

	const userID, vehicleID, vin = "user_susp_td", "veh_susp_td", "5YJ3E1EA1PF000310"
	seedOwnerUser(t, userID, "Owner", userID+"@example.com")
	seedTeardownVehicle(t, vehicleID, userID, vin)

	ctx := context.Background()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_vehicle_telemetry_suspensions (vehicle_id, warned_at, suspended_at)
		 VALUES ($1, NOW() - INTERVAL '25 hours', NOW() - INTERVAL '1 hour')`, vehicleID); err != nil {
		t.Fatalf("seed suspension: %v", err)
	}

	if _, err := newTestTeardown().RemoveVehicle(ctx, userID, vehicleID); err != nil {
		t.Fatalf("RemoveVehicle: %v", err)
	}

	var n int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM go_vehicle_telemetry_suspensions WHERE vehicle_id = $1`, vehicleID).Scan(&n); err != nil {
		t.Fatalf("count suspensions: %v", err)
	}
	if n != 0 {
		t.Errorf("suspension rows after teardown = %d, want 0 — an unlinked car must not leave a "+
			"disconnection stamp behind it", n)
	}
}

// TestOwnerTeardown_ClearsFleetConfigAttempts pins the MYR-593 sibling fix.
//
// Migration 0031's header has claimed since it landed that "a deleted car's row
// is swept by the same DELETE path when the vehicle is torn down". No such
// statement existed, in this transaction or anywhere else, so every torn-down
// car that had ever failed a config push left its retry schedule behind — and
// the table the migration calls self-draining was quietly accumulating a row
// per removed car, forever. Same shape as the suspension row above: Go-owned,
// keyed by vehicle id, no FK to cascade it (CG-DL-9), so the DELETE is explicit
// or it does not happen.
func TestOwnerTeardown_ClearsFleetConfigAttempts(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ensureTeardownSchema(t)
	cleanTeardownTables(t)

	const userID, vehicleID, vin = "user_fca_td", "veh_fca_td", "5YJ3E1EA1PF000312"
	const keptID, keptVIN = "veh_fca_keep", "5YJ3E1EA1PF000313"
	seedOwnerUser(t, userID, "Owner", userID+"@example.com")
	seedTeardownVehicle(t, vehicleID, userID, vin)
	// A second car, so this is a mid-fleet removal and the assertion below can
	// prove the DELETE is keyed on the removed vehicle rather than on the owner.
	seedTeardownVehicle(t, keptID, userID, keptVIN)

	ctx := context.Background()
	for _, id := range []string{vehicleID, keptID} {
		if _, err := testPool.Exec(ctx,
			`INSERT INTO go_fleet_config_attempts (vehicle_id, attempt_count, last_outcome)
			 VALUES ($1, 4, 'awaiting_virtual_key')`, id); err != nil {
			t.Fatalf("seed fleet-config attempt for %s: %v", id, err)
		}
	}

	if _, err := newTestTeardown().RemoveVehicle(ctx, userID, vehicleID); err != nil {
		t.Fatalf("RemoveVehicle: %v", err)
	}

	var gone int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM go_fleet_config_attempts WHERE vehicle_id = $1`, vehicleID).Scan(&gone); err != nil {
		t.Fatalf("count attempts for the removed car: %v", err)
	}
	if gone != 0 {
		t.Errorf("fleet-config attempt rows for the removed car = %d, want 0 — migration 0031 promises "+
			"this sweep and the table is only self-draining if it happens", gone)
	}

	var kept int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM go_fleet_config_attempts WHERE vehicle_id = $1`, keptID).Scan(&kept); err != nil {
		t.Fatalf("count attempts for the kept car: %v", err)
	}
	if kept != 1 {
		t.Errorf("fleet-config attempt rows for the KEPT car = %d, want 1 — the delete must be keyed on "+
			"the removed vehicle, not on its owner", kept)
	}
}

// TestOwnerTeardown_NoSuspensionIsStillClean is the overwhelmingly common case:
// the vast majority of teardowns are of cars that were never suspended, so the
// DELETE has to be a clean no-op rather than something the transaction notices.
func TestOwnerTeardown_NoSuspensionIsStillClean(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable")
	}
	ensureTeardownSchema(t)
	cleanTeardownTables(t)

	const userID, vehicleID, vin = "user_nosusp_td", "veh_nosusp_td", "5YJ3E1EA1PF000311"
	seedOwnerUser(t, userID, "Owner", userID+"@example.com")
	seedTeardownVehicle(t, vehicleID, userID, vin)

	res, err := newTestTeardown().RemoveVehicle(context.Background(), userID, vehicleID)
	if err != nil {
		t.Fatalf("RemoveVehicle: %v", err)
	}
	if !res.Removed {
		t.Errorf("result = %+v, want Removed", res)
	}
}
