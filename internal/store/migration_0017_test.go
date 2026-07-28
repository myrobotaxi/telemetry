package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// migration0017Columns is every column migration 0017 adds, with the type the
// up-migration declares. TIMESTAMPTZ ("timestamp with time zone") is asserted
// rather than merely presence: a naive TIMESTAMP would be actively wrong here,
// because a service centre's estimate crosses a DST boundary about twice a year
// and the scheduler compares this value against an absolute rider-supplied
// scheduledFor.
var migration0017Columns = map[string]string{
	"service_etc":             "timestamp with time zone",
	"service_expected_end_at": "timestamp with time zone",
}

// serviceWindowColumnTypes reads the live column types + nullability for the
// two MYR-316 columns. Separate from migration_0015_test.go's columnTypes so
// each migration's test owns its own nullability assertion.
func serviceWindowColumnTypes(t *testing.T, pool *pgxpool.Pool) map[string]string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT column_name, data_type, is_nullable
		 FROM information_schema.columns
		 WHERE table_name = 'go_vehicle_control_state'`)
	if err != nil {
		t.Fatalf("query information_schema: %v", err)
	}
	defer rows.Close()

	got := make(map[string]string)
	for rows.Next() {
		var name, dataType, nullable string
		if err := rows.Scan(&name, &dataType, &nullable); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if _, ours := migration0017Columns[name]; ours && nullable != "YES" {
			t.Errorf("%s: is_nullable = %s, want YES — a NULL here means "+
				"'no estimate from this source', which is the common case", name, nullable)
		}
		got[name] = dataType
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return got
}

// TestMigration0017_UpAddsServiceWindowColumns verifies the up-migration lands
// both columns with the right type and nullability.
func TestMigration0017_UpAddsServiceWindowColumns(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	got := serviceWindowColumnTypes(t, testPool)
	for col, wantType := range migration0017Columns {
		actual, ok := got[col]
		if !ok {
			t.Errorf("column %s missing after migrate up", col)
			continue
		}
		if actual != wantType {
			t.Errorf("column %s: data_type = %q, want %q", col, actual, wantType)
		}
	}
}

// TestMigration0017_DownDropsServiceWindowColumnsOnly exercises the rollback
// and proves it is SURGICAL: only the two MYR-316 columns go, and every column
// the earlier control-state migrations installed on the same table survives.
//
// Migrates down TO VERSION 16 and back up, leaving the database at head for
// whatever runs next. Version-targeted rather than Steps(-1) on purpose: a
// relative step silently rolls back whatever the newest migration happens to
// be, so the next issue that adds one would turn this into a test of ITS
// down-migration (MYR-179 hit exactly that when 0016 landed).
func TestMigration0017_DownDropsServiceWindowColumnsOnly(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	m := newTestMigrator(t)
	defer func() { _, _ = m.Close() }()

	if err := m.Migrate(16); err != nil {
		t.Fatalf("migrate down to 16: %v", err)
	}
	// Restore the schema no matter how the assertions below go, so whatever
	// runs next still sees a head database.
	t.Cleanup(func() {
		if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
			t.Fatalf("restore migrations to head: %v", err)
		}
	})

	got := serviceWindowColumnTypes(t, testPool)
	for col := range migration0017Columns {
		if _, still := got[col]; still {
			t.Errorf("column %s survived the down-migration", col)
		}
	}

	// The rollback must not take the rest of the control-state family with it.
	for _, survivor := range []string{
		"is_locked",               // MYR-269, migration 0008
		"media_volume",            // MYR-273, migration 0010
		"trim",                    // MYR-279, migration 0011
		"hvac_auto_mode",          // MYR-274, migration 0012
		"seat_vent_enabled",       // MYR-298, migration 0014
		"media_now_playing_title", // MYR-303, migration 0015
		"seat_cooling_capable",    // MYR-308, migration 0015
		"updated_at",              // the table's own bookkeeping column
	} {
		if _, ok := got[survivor]; !ok {
			t.Errorf("column %s was dropped by the 0017 down-migration — it must be surgical", survivor)
		}
	}
}
