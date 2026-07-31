package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-172 migration 0025: go_live_activities, the ActivityKit push-token
// registry behind the rider's Live Activity.

// migration0025Columns is the shape 0025 installs, with the information_schema
// data_type each column must report.
//
// The TYPES are asserted, not just presence. ended_at being a TIMESTAMPTZ
// rather than a boolean is the design — it records WHEN, which is the first
// question asked when a rider reports a stuck lock screen — and a future edit
// that "simplified" it to a flag would lose that answer silently.
var migration0025Columns = map[string]string{
	"id":                  "text",
	"ride_request_id":     "text",
	"user_id":             "text",
	"activity_push_token": "text",
	"sandbox":             "boolean",
	"created_at":          "timestamp with time zone",
	"updated_at":          "timestamp with time zone",
	"ended_at":            "timestamp with time zone",
}

// liveActivityColumnTypes introspects the installed go_live_activities columns.
func liveActivityColumnTypes(t *testing.T) map[string]string {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		`SELECT column_name, data_type
		 FROM information_schema.columns
		 WHERE table_name = 'go_live_activities'`)
	if err != nil {
		t.Fatalf("introspect go_live_activities columns: %v", err)
	}
	defer rows.Close()

	got := map[string]string{}
	for rows.Next() {
		var name, dataType string
		if err := rows.Scan(&name, &dataType); err != nil {
			t.Fatalf("scan column row: %v", err)
		}
		got[name] = dataType
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate column rows: %v", err)
	}
	return got
}

// liveActivityNullability reports information_schema.is_nullable per column.
func liveActivityNullability(t *testing.T) map[string]string {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		`SELECT column_name, is_nullable
		 FROM information_schema.columns
		 WHERE table_name = 'go_live_activities'`)
	if err != nil {
		t.Fatalf("introspect go_live_activities nullability: %v", err)
	}
	defer rows.Close()

	got := map[string]string{}
	for rows.Next() {
		var name, nullable string
		if err := rows.Scan(&name, &nullable); err != nil {
			t.Fatalf("scan column row: %v", err)
		}
		got[name] = nullable
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate column rows: %v", err)
	}
	return got
}

// cleanGoLiveActivities empties the table and the rides these cases seed.
//
// The rides go too, not just the Activities: migration 0004's partial unique
// index admits ONE open instant ride per rider, so a leftover fixture from a
// previous case collides with the next one on a constraint that has nothing to
// do with this migration.
func cleanGoLiveActivities(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if _, err := testPool.Exec(ctx, `DELETE FROM go_live_activities`); err != nil {
		t.Fatalf("clean go_live_activities: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`DELETE FROM go_ride_requests WHERE id LIKE 'cride0025%'`); err != nil {
		t.Fatalf("clean 0025 fixture rides: %v", err)
	}
}

// seedActivityRide inserts the minimum ride row an Activity can hang off.
// The FK is this migration's one novelty, so the fixtures must go through a
// real ride rather than an invented id.
//
// The rider AND vehicle ids are DERIVED from the ride id so that two rides
// seeded in one case involve two different people and two different cars.
// Migration 0004 permits a rider only one open instant ride at a time and 0013
// does the same per vehicle; sharing either across fixtures fails one of those
// constraints rather than the one under test.
func seedActivityRide(t *testing.T, rideID string) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO go_ride_requests
		   (id, rider_id, owner_id, vehicle_id,
		    pickup_lat_enc, pickup_lng_enc, pickup_label,
		    dropoff_lat_enc, dropoff_lng_enc, dropoff_label, status)
		 VALUES ($1, 'rider_' || $1, 'cowner0025', 'veh_' || $1,
		         'enc', 'enc', 'Pickup',
		         'enc', 'enc', 'Home', 'accepted')
		 ON CONFLICT (id) DO NOTHING`, rideID); err != nil {
		t.Fatalf("seed ride %s: %v", rideID, err)
	}
}

func TestMigration0025_UpCreatesLiveActivities(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)

	if !tableExists(t, "go_live_activities") {
		t.Fatal("go_live_activities missing after migrate up")
	}

	got := liveActivityColumnTypes(t)
	for col, wantType := range migration0025Columns {
		actual, ok := got[col]
		if !ok {
			t.Errorf("column %s missing after migrate up", col)
			continue
		}
		if actual != wantType {
			t.Errorf("column %s: data_type = %q, want %q", col, actual, wantType)
		}
	}
	for col := range got {
		if _, documented := migration0025Columns[col]; !documented {
			t.Errorf("undocumented column %s on go_live_activities; add it to migration0025Columns and to data-classification.md §1.18", col)
		}
	}

	// NULLABILITY IS LOAD-BEARING IN OPPOSITE DIRECTIONS.
	nullable := liveActivityNullability(t)
	if nullable["activity_push_token"] != "NO" {
		t.Error("activity_push_token is NULLABLE — a row with no token addresses nothing, " +
			"and there is no honest-unknown state: a caller with no token simply does not register")
	}
	if nullable["ended_at"] != "YES" {
		t.Error("ended_at is NOT NULL — NULL is the ordinary active state, which is what " +
			"leaves a freshly upserted row correct untouched")
	}
}

// TestMigration0025_UpEnforcesOneActivityPerRideAndParty pins the natural key.
//
// It is (ride_request_id, user_id) and NOT the token, because ActivityKit
// rotates the update token during the life of one Activity: keying on the token
// would accumulate a row per rotation and leave the sender guessing which is
// live.
func TestMigration0025_UpEnforcesOneActivityPerRideAndParty(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanGoLiveActivities(t)
	seedActivityRide(t, "cride0025a")
	ctx := context.Background()

	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_live_activities (id, ride_request_id, user_id, activity_push_token)
		 VALUES ('cact0025a', 'cride0025a', 'crider0025', 'token-one')`); err != nil {
		t.Fatalf("seed first activity: %v", err)
	}

	_, err := testPool.Exec(ctx,
		`INSERT INTO go_live_activities (id, ride_request_id, user_id, activity_push_token)
		 VALUES ('cact0025b', 'cride0025a', 'crider0025', 'token-two')`)
	if err == nil {
		t.Fatal("a second Activity for the same (ride, party) was accepted; the unique constraint is missing")
	}

	// The SAME token on a DIFFERENT ride must be fine — the token is not the
	// key, and a phone can legitimately be mid-rotation across two rides.
	seedActivityRide(t, "cride0025b")
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_live_activities (id, ride_request_id, user_id, activity_push_token)
		 VALUES ('cact0025c', 'cride0025b', 'crider0025', 'token-one')`); err != nil {
		t.Fatalf("same token on a different ride was rejected: %v", err)
	}
}

// TestMigration0025_UpDefaultsAreTheActiveState pins the defaults: a freshly
// inserted row is live, in production, and stamped.
func TestMigration0025_UpDefaultsAreTheActiveState(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanGoLiveActivities(t)
	seedActivityRide(t, "cride0025c")
	ctx := context.Background()

	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_live_activities (id, ride_request_id, user_id, activity_push_token)
		 VALUES ('cact0025d', 'cride0025c', 'crider0025', 'token-default')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var sandbox bool
	var endedAt *string
	var createdAt, updatedAt time.Time
	if err := testPool.QueryRow(ctx,
		`SELECT sandbox, ended_at::text, created_at, updated_at
		 FROM go_live_activities WHERE id = 'cact0025d'`,
	).Scan(&sandbox, &endedAt, &createdAt, &updatedAt); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if sandbox {
		t.Error("sandbox defaulted to TRUE — production is the default gateway, " +
			"and a wrong guess here is rejected by Apple as BadDeviceToken")
	}
	if endedAt != nil {
		t.Error("ended_at defaulted to non-NULL — a new registration must never be born ended")
	}
	if createdAt.IsZero() || updatedAt.IsZero() {
		t.Error("timestamps were not defaulted")
	}
}

// TestMigration0025_UpCascadesFromTheRide is the one novelty in this migration:
// the first foreign key in the go_ namespace.
//
// It earns its place because the ride already has hard-delete paths (owner
// teardown, account deletion) and a token addressing an Activity for a ride
// that no longer exists is unreachable garbage. The alternative — an unenforced
// pointer plus a bespoke delete in every teardown path — is three places to
// forget instead of zero.
func TestMigration0025_UpCascadesFromTheRide(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanGoLiveActivities(t)
	seedActivityRide(t, "cride0025d")
	ctx := context.Background()

	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_live_activities (id, ride_request_id, user_id, activity_push_token)
		 VALUES ('cact0025e', 'cride0025d', 'crider0025', 'token-cascade')`); err != nil {
		t.Fatalf("seed activity: %v", err)
	}

	// A token pointing at a ride that does not exist must be refused outright.
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_live_activities (id, ride_request_id, user_id, activity_push_token)
		 VALUES ('cact0025f', 'cride-does-not-exist', 'crider0025', 'token-orphan')`); err == nil {
		t.Error("an Activity for a non-existent ride was accepted; the foreign key is missing")
	}

	if _, err := testPool.Exec(ctx, `DELETE FROM go_ride_requests WHERE id = 'cride0025d'`); err != nil {
		t.Fatalf("delete ride: %v", err)
	}

	var remaining int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM go_live_activities WHERE ride_request_id = 'cride0025d'`).Scan(&remaining); err != nil {
		t.Fatalf("count after ride delete: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d activity rows survived their ride's deletion; ON DELETE CASCADE is missing", remaining)
	}
}

// TestMigration0025_DownDropsOnlyItsOwnTable RUNS THE REAL MIGRATION FILES —
// both of them — rather than asserting against a retyped copy.
//
// It rolls the schema back to 24 and forward again, letting golang-migrate
// execute 0025_live_activities.down.sql and .up.sql itself. That matters most
// for the FOREIGN KEY: a down-migration that dropped the table while leaving a
// constraint behind, or an up that failed to re-create it, would be invisible
// to a test that only introspected a schema built once. golang-migrate is the
// thing that will run these files in production, so it is the path worth
// exercising.
func TestMigration0025_DownDropsOnlyItsOwnTable(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanGoLiveActivities(t)

	m := newTestMigrator(t)
	defer func() { _, _ = m.Close() }()

	// Restore the schema no matter how the assertions below go, so whatever
	// runs next still sees a head database.
	t.Cleanup(func() {
		if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
			t.Fatalf("restore migrations to head: %v", err)
		}
	})

	if err := m.Migrate(24); err != nil {
		t.Fatalf("migrate down to 24: %v", err)
	}
	if tableExists(t, "go_live_activities") {
		t.Error("go_live_activities survived the down-migration")
	}
	// The ride table it pointed at must be untouched — the rollback is surgical
	// to the one table 0025 added.
	if !tableExists(t, "go_ride_requests") {
		t.Fatal("the down-migration took go_ride_requests with it")
	}

	if err := m.Migrate(25); err != nil {
		t.Fatalf("migrate back up to 25: %v", err)
	}
	if !tableExists(t, "go_live_activities") {
		t.Fatal("go_live_activities missing after re-applying the up-migration")
	}

	// Re-assert the constraint after the round trip: this is what proves the
	// .up.sql file itself installs the FK, not merely that some earlier run did.
	seedActivityRide(t, "cride0025e")
	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO go_live_activities (id, ride_request_id, user_id, activity_push_token)
		 VALUES ('cact0025g', 'cride-still-missing', 'crider0025', 'token-orphan-2')`); err == nil {
		t.Error("the foreign key did not survive a down/up cycle")
	}
}
