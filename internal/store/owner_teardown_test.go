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
	for _, tbl := range []string{`go_ride_requests`, `"TripStop"`, `"Drive"`, `"Vehicle"`, `"Account"`, `"Settings"`, `"User"`} {
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
		metadata                                 []byte
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
