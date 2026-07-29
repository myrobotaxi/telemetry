package store_test

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// migration0019Columns is the go_push_devices shape MYR-186 installs, with the
// information_schema data_type each column must report.
var migration0019Columns = map[string]string{
	"id":           "text",
	"user_id":      "text",
	"device_token": "text",
	"platform":     "text",
	"sandbox":      "boolean",
	"created_at":   "timestamp with time zone",
	"last_seen_at": "timestamp with time zone",
}

// pushDeviceColumnTypes introspects the installed go_push_devices columns.
func pushDeviceColumnTypes(t *testing.T) map[string]string {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		`SELECT column_name, data_type
		 FROM information_schema.columns
		 WHERE table_name = 'go_push_devices'`)
	if err != nil {
		t.Fatalf("introspect go_push_devices columns: %v", err)
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

// tableExists reports whether a relation is currently installed.
func tableExists(t *testing.T, name string) bool {
	t.Helper()
	var exists bool
	if err := testPool.QueryRow(context.Background(),
		`SELECT to_regclass($1) IS NOT NULL`, name).Scan(&exists); err != nil {
		t.Fatalf("to_regclass(%s): %v", name, err)
	}
	return exists
}

func TestMigration0019_UpCreatesPushDevices(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	got := pushDeviceColumnTypes(t)
	for col, wantType := range migration0019Columns {
		actual, ok := got[col]
		if !ok {
			t.Errorf("column %s missing after migrate up", col)
			continue
		}
		if actual != wantType {
			t.Errorf("column %s: data_type = %q, want %q", col, actual, wantType)
		}
	}
	if len(got) != len(migration0019Columns) {
		t.Errorf("go_push_devices has %d columns, want %d — an undocumented column is a classification gap",
			len(got), len(migration0019Columns))
	}
}

// TestMigration0019_UpEnforcesTokenUniqueness proves the constraint the whole
// re-parenting upsert depends on. Without it ON CONFLICT (device_token) has no
// arbiter and the registration endpoint fails at runtime, not at deploy.
func TestMigration0019_UpEnforcesTokenUniqueness(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanPushDevices(t)
	ctx := context.Background()

	const token = "token-migration-unique"
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_push_devices (id, user_id, device_token) VALUES ($1,$2,$3)`,
		"cmig0019a", "cmiguser001", token); err != nil {
		t.Fatalf("seed first row: %v", err)
	}
	_, err := testPool.Exec(ctx,
		`INSERT INTO go_push_devices (id, user_id, device_token) VALUES ($1,$2,$3)`,
		"cmig0019b", "cmiguser002", token)
	if err == nil {
		t.Fatal("second insert of the same device_token succeeded, want a unique violation")
	}
}

// TestMigration0019_UpRejectsUnknownPlatform pins the CHECK constraint: v1 is
// iOS-only and adding a platform is a deliberate schema change, not a write.
func TestMigration0019_UpRejectsUnknownPlatform(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanPushDevices(t)

	_, err := testPool.Exec(context.Background(),
		`INSERT INTO go_push_devices (id, user_id, device_token, platform) VALUES ($1,$2,$3,$4)`,
		"cmig0019c", "cmiguser003", "token-migration-platform", "android")
	if err == nil {
		t.Fatal("insert with platform='android' succeeded, want a CHECK violation")
	}
}

// TestMigration0019_DownDropsPushDevicesOnly exercises the rollback and proves
// it is SURGICAL: go_push_devices goes and the Go-owned tables installed by
// earlier migrations survive.
//
// Migrates down TO VERSION 18 and back up, leaving the database at head for
// whatever runs next. Version-targeted rather than Steps(-1) on purpose: a
// relative step silently rolls back whatever the newest migration happens to
// be, so the next issue that adds one would turn this into a test of ITS
// down-migration.
func TestMigration0019_DownDropsPushDevicesOnly(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	m := newTestMigrator(t)
	defer func() { _, _ = m.Close() }()

	if err := m.Migrate(18); err != nil {
		t.Fatalf("migrate down to 18: %v", err)
	}
	// Restore the schema no matter how the assertions below go, so whatever
	// runs next still sees a head database.
	t.Cleanup(func() {
		if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
			t.Fatalf("restore migrations to head: %v", err)
		}
	})

	if tableExists(t, "go_push_devices") {
		t.Error("go_push_devices survived the down-migration")
	}

	// The rollback must not take the rest of the Go-owned schema with it.
	for _, survivor := range []string{
		"go_ride_requests",         // MYR-173, migration 0002
		"go_users",                 // MYR-193, migration 0003
		"go_removed_vehicles",      // MYR-261, migration 0006
		"go_vehicle_control_state", // MYR-269, migration 0008
	} {
		if !tableExists(t, survivor) {
			t.Errorf("table %s was dropped by the 0019 down-migration — it must be surgical", survivor)
		}
	}
}
