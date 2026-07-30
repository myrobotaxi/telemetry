package store_test

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-342 migration 0021: ride_share_enabled on go_vehicle_control_state.
//
// The assertions here are deliberately about NULLABILITY and DEFAULT rather
// than mere presence, because those two are the whole design. Every other
// column on this table is nullable honest-unknown; this one is NOT NULL DEFAULT
// true, and if either half regressed the feature would fail silently in the
// worst direction:
//
//   - drop NOT NULL and a NULL becomes possible, which the readers COALESCE to
//     true — indistinguishable from enabled, so an owner's pause could vanish;
//   - drop the DEFAULT and the ALTER would either fail on a non-empty table or
//     backfill NULLs, and a fresh INSERT that omitted the column would fail
//     outright, taking the control-state upsert down with it.

// migration0021ColumnState is the column's declared shape: type, nullability,
// and default expression.
type migration0021ColumnState struct {
	dataType string
	nullable string
	def      string
}

// rideShareColumnState reads the live type, nullability and default for the
// MYR-342 column. Separate from the 0015/0017 helpers so each migration's test
// owns its own assertions.
func rideShareColumnState(t *testing.T) (migration0021ColumnState, bool) {
	t.Helper()
	var got migration0021ColumnState
	var def *string
	err := testPool.QueryRow(context.Background(),
		`SELECT data_type, is_nullable, column_default
		 FROM information_schema.columns
		 WHERE table_name = 'go_vehicle_control_state'
		   AND column_name = 'ride_share_enabled'`).Scan(&got.dataType, &got.nullable, &def)
	if err != nil {
		return migration0021ColumnState{}, false
	}
	if def != nil {
		got.def = *def
	}
	return got, true
}

// TestMigration0021_UpAddsRideShareColumn verifies the up-migration lands the
// column as a NOT NULL boolean defaulting to true.
func TestMigration0021_UpAddsRideShareColumn(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	got, ok := rideShareColumnState(t)
	if !ok {
		t.Fatal("column ride_share_enabled missing after migrate up")
	}
	if got.dataType != "boolean" {
		t.Errorf("data_type = %q, want %q", got.dataType, "boolean")
	}
	if got.nullable != "NO" {
		t.Errorf("is_nullable = %q, want NO — there is no unknown state for this "+
			"field, and a NULL would COALESCE to enabled, silently discarding a pause", got.nullable)
	}
	if got.def != "true" {
		t.Errorf("column_default = %q, want %q — the default is what backfills every "+
			"pre-MYR-342 row to the correct behaviour (all cars were shareable)", got.def, "true")
	}
}

// TestMigration0021_ExistingRowsBackfillToEnabled proves the DEFAULT actually
// reaches rows that predate the column, which is the migration's real job: a
// car that already had control state must come out of the migration ENABLED,
// never paused.
func TestMigration0021_ExistingRowsBackfillToEnabled(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	ctx := context.Background()

	m := newTestMigrator(t)
	defer func() { _, _ = m.Close() }()

	// Roll back to the pre-0021 schema, seed a control-state row as a
	// pre-MYR-342 deployment would have, then migrate forward again.
	if err := m.Migrate(20); err != nil {
		t.Fatalf("migrate down to 20: %v", err)
	}
	t.Cleanup(func() {
		if err := store.RunMigrations(ctx, testConnStr, testLogger()); err != nil {
			t.Fatalf("restore migrations to head: %v", err)
		}
	})

	const legacyVehicle = "clmigration0021legacyrow"
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_vehicle_control_state (vehicle_id, is_locked) VALUES ($1, true)
		 ON CONFLICT (vehicle_id) DO NOTHING`, legacyVehicle); err != nil {
		t.Fatalf("seed pre-0021 control-state row: %v", err)
	}
	t.Cleanup(func() {
		_, _ = testPool.Exec(ctx, `DELETE FROM go_vehicle_control_state WHERE vehicle_id = $1`, legacyVehicle)
	})

	if err := store.RunMigrations(ctx, testConnStr, testLogger()); err != nil {
		t.Fatalf("migrate back up: %v", err)
	}

	var enabled bool
	if err := testPool.QueryRow(ctx,
		`SELECT ride_share_enabled FROM go_vehicle_control_state WHERE vehicle_id = $1`,
		legacyVehicle).Scan(&enabled); err != nil {
		t.Fatalf("read back the backfilled row: %v", err)
	}
	if !enabled {
		t.Error("a row that predates the column came out of the migration PAUSED — " +
			"every pre-MYR-342 car was shareable and must stay so")
	}
}

// TestMigration0021_DownDropsRideShareColumnOnly exercises the rollback and
// proves it is SURGICAL: only the MYR-342 column goes, and every column the
// earlier control-state migrations installed on the same table survives.
//
// Migrates down TO VERSION 20 and back up, leaving the database at head.
// Version-targeted rather than Steps(-1) on purpose: a relative step silently
// rolls back whatever the newest migration happens to be, so the next issue
// that adds one would turn this into a test of ITS down-migration (MYR-179 hit
// exactly that when 0016 landed).
func TestMigration0021_DownDropsRideShareColumnOnly(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}

	m := newTestMigrator(t)
	defer func() { _, _ = m.Close() }()

	if err := m.Migrate(20); err != nil {
		t.Fatalf("migrate down to 20: %v", err)
	}
	t.Cleanup(func() {
		if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
			t.Fatalf("restore migrations to head: %v", err)
		}
	})

	if _, still := rideShareColumnState(t); still {
		t.Error("column ride_share_enabled survived the down-migration")
	}

	// The rollback must not take the rest of the control-state family with it.
	got := serviceWindowColumnTypes(t, testPool)
	for _, survivor := range []string{
		"is_locked",               // MYR-269, migration 0008
		"media_volume",            // MYR-273, migration 0010
		"trim",                    // MYR-279, migration 0011
		"hvac_auto_mode",          // MYR-274, migration 0012
		"seat_vent_enabled",       // MYR-298, migration 0014
		"media_now_playing_title", // MYR-303, migration 0015
		"seat_cooling_capable",    // MYR-308, migration 0015
		"service_etc",             // MYR-316, migration 0017
		"trim_label",              // MYR-320, migration 0018
		"updated_at",              // the table's own bookkeeping column
	} {
		if _, ok := got[survivor]; !ok {
			t.Errorf("column %s was dropped by the 0021 down-migration — it must be surgical", survivor)
		}
	}
}
