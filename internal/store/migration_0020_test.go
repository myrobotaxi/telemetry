package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// migration0020Columns is the go_vehicle_shares shape MYR-184 installs, with
// the information_schema data_type each column must report.
//
// It describes what 0020 ITSELF installs, not the table at head — later
// migrations extend the table and document their own additions in their own
// column maps (MYR-369 / 0024 adds allow_rides + suspended_at). The
// undocumented-column guard below counts against the UNION of those maps, so a
// column that belongs to no migration's map is still caught.
var migration0020Columns = map[string]string{
	"id":                  "text",
	"vehicle_id":          "text",
	"owner_user_id":       "text",
	"label":               "text",
	"permission":          "text",
	"code":                "text",
	"status":              "text",
	"created_at":          "timestamp with time zone",
	"expires_at":          "timestamp with time zone",
	"accepted_at":         "timestamp with time zone",
	"accepted_by_user_id": "text",
	"revoked_at":          "timestamp with time zone",
}

// vehicleShareColumnTypes introspects the installed go_vehicle_shares columns.
func vehicleShareColumnTypes(t *testing.T) map[string]string {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		`SELECT column_name, data_type
		 FROM information_schema.columns
		 WHERE table_name = 'go_vehicle_shares'`)
	if err != nil {
		t.Fatalf("introspect go_vehicle_shares columns: %v", err)
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

// insertShare seeds one raw row, bypassing the repository so the test can
// construct states the repository would refuse to write.
func insertShare(t *testing.T, id, vehicleID, ownerID, code, status string, expiresAt time.Time, acceptedBy *string) error {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`INSERT INTO go_vehicle_shares
		   (id, vehicle_id, owner_user_id, label, permission, code, status, expires_at, accepted_by_user_id)
		 VALUES ($1, $2, $3, 'Test Recipient', 'live', $4, $5, $6, $7)`,
		id, vehicleID, ownerID, code, status, expiresAt, acceptedBy)
	return err
}

// cleanVehicleShares empties the grant table between tests.
func cleanVehicleShares(t *testing.T) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(), `DELETE FROM go_vehicle_shares`); err != nil {
		t.Fatalf("clean go_vehicle_shares: %v", err)
	}
}

func TestMigration0020_UpCreatesVehicleShares(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)

	got := vehicleShareColumnTypes(t)
	for col, wantType := range migration0020Columns {
		actual, ok := got[col]
		if !ok {
			t.Errorf("column %s missing after migrate up", col)
			continue
		}
		if actual != wantType {
			t.Errorf("column %s: data_type = %q, want %q", col, actual, wantType)
		}
	}
	documented := len(migration0020Columns) + len(migration0024Columns)
	if len(got) != documented {
		t.Errorf("go_vehicle_shares has %d columns, want %d — an undocumented column is a classification gap",
			len(got), documented)
	}
}

// TestMigration0020_UpRejectsUnknownEnums pins both CHECK constraints. A tier
// or a status outside the enum must be a schema-level refusal, not something
// the application layer is trusted to prevent — a bad `permission` would be
// evaluated by AtLeast and silently deny, and a bad `status` would make a row
// invisible to every query without ever being flagged.
func TestMigration0020_UpRejectsUnknownEnums(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanVehicleShares(t)

	future := time.Now().Add(24 * time.Hour)

	t.Run("unknown permission", func(t *testing.T) {
		_, err := testPool.Exec(context.Background(),
			`INSERT INTO go_vehicle_shares
			   (id, vehicle_id, owner_user_id, label, permission, code, expires_at)
			 VALUES ('csh0020a', 'veh1', 'own1', 'L', 'admin', 'AAAAAA', $1)`, future)
		if err == nil {
			t.Fatal("insert with permission='admin' succeeded, want a CHECK violation")
		}
	})

	t.Run("unknown status", func(t *testing.T) {
		err := insertShare(t, "csh0020b", "veh1", "own1", "BBBBBB", "expired", future, nil)
		if err == nil {
			t.Fatal("insert with status='expired' succeeded, want a CHECK violation")
		}
	})
}

// TestMigration0020_UpEnforcesOneAcceptedGrantPerPair pins the partial-unique
// index that makes redeem idempotent. Without it a retried redemption, or two
// invites for the same car, would stack duplicate grants on one viewer — and
// the redeem path's ErrShareAlreadyGranted branch would be unreachable.
func TestMigration0020_UpEnforcesOneAcceptedGrantPerPair(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanVehicleShares(t)

	future := time.Now().Add(24 * time.Hour)
	viewer := "viewer-0020"

	if err := insertShare(t, "csh0020c", "veh-share", "own1", "CCCCCC", "accepted", future, &viewer); err != nil {
		t.Fatalf("seed first accepted grant: %v", err)
	}
	if err := insertShare(t, "csh0020d", "veh-share", "own1", "DDDDDD", "accepted", future, &viewer); err == nil {
		t.Fatal("second accepted grant for the same (viewer, vehicle) succeeded, want a unique violation")
	}

	// The index is PARTIAL on status='accepted': a revoked tombstone for the
	// same pair must not block a fresh grant, otherwise revoking somebody
	// would permanently bar re-inviting them.
	if err := insertShare(t, "csh0020e", "veh-share2", "own1", "EEEEEE", "revoked", future, &viewer); err != nil {
		t.Fatalf("seed revoked tombstone: %v", err)
	}
	if err := insertShare(t, "csh0020f", "veh-share2", "own1", "FFFFFF", "accepted", future, &viewer); err != nil {
		t.Fatalf("re-granting after a revoke was blocked: %v", err)
	}

	// And two DIFFERENT people may both hold a grant on one car.
	other := "viewer-0020-other"
	if err := insertShare(t, "csh0020g", "veh-share", "own1", "GGGGGG", "accepted", future, &other); err != nil {
		t.Fatalf("second viewer on the same vehicle was blocked: %v", err)
	}
}

// TestMigration0020_DownDropsVehicleShares targets the EXPLICIT version rather
// than Steps(-1): stepping back one is whatever happens to be at the head when
// the test runs, which silently tests a different migration the moment 0021
// lands.
func TestMigration0020_DownDropsVehicleShares(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanVehicleShares(t)

	if !tableExists(t, "go_vehicle_shares") {
		t.Fatal("go_vehicle_shares missing before the down test")
	}

	m := newTestMigrator(t)
	defer func() { _, _ = m.Close() }()

	if err := m.Migrate(19); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		t.Fatalf("migrate down to 0019: %v", err)
	}
	// Restore the schema however the assertions below go, so whatever runs
	// next still sees a head database.
	t.Cleanup(func() {
		if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
			t.Fatalf("restore migrations to head: %v", err)
		}
	})

	if tableExists(t, "go_vehicle_shares") {
		t.Error("go_vehicle_shares still present after migrating down to 0019")
	}
	// The rollback must take ONLY the sharing table with it — 0019's
	// registry is a sibling, not a dependency.
	if !tableExists(t, "go_push_devices") {
		t.Error("go_push_devices was dropped by the 0020 down-migration")
	}
}
