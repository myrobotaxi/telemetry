package store_test

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-369 migration 0024: the per-grant capability flags on go_vehicle_shares.

// migration0024Columns is the shape 0024 ADDS, with the information_schema
// data_type each must report.
//
// The TYPES are asserted, not just presence. `suspended_at` being a TIMESTAMPTZ
// rather than a boolean is the design: it records WHEN, which a boolean cannot,
// and NULL is the ordinary active state so every pre-existing row is correct
// untouched. A future edit that "simplified" it to a boolean would lose the
// audit answer to "when did I cut them off?" and this map is what stops it.
var migration0024Columns = map[string]string{
	"allow_rides":  "boolean",
	"suspended_at": "timestamp with time zone",
}

// shareColumnNullability reports information_schema.is_nullable per column.
//
// Split from the type introspection (vehicleShareColumnTypes, migration_0020_test.go)
// rather than duplicating it: nullability is the one property these two columns
// disagree on, and it is load-bearing in OPPOSITE directions for each.
func shareColumnNullability(t *testing.T) map[string]string {
	t.Helper()
	rows, err := testPool.Query(context.Background(),
		`SELECT column_name, is_nullable
		 FROM information_schema.columns
		 WHERE table_name = 'go_vehicle_shares'`)
	if err != nil {
		t.Fatalf("introspect go_vehicle_shares nullability: %v", err)
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

func TestMigration0024_UpAddsGrantFlags(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)

	got := vehicleShareColumnTypes(t)
	for col, wantType := range migration0024Columns {
		actual, ok := got[col]
		if !ok {
			t.Errorf("column %s missing after migrate up", col)
			continue
		}
		if actual != wantType {
			t.Errorf("column %s: data_type = %q, want %q", col, actual, wantType)
		}
	}

	// NULLABILITY IS LOAD-BEARING IN OPPOSITE DIRECTIONS HERE.
	nullable := shareColumnNullability(t)
	if nullable["allow_rides"] != "NO" {
		t.Error("allow_rides is NULLABLE — there is no unknown state for a capability, " +
			"and a nullable column would invent a third one every reader would collapse anyway")
	}
	if nullable["suspended_at"] != "YES" {
		t.Error("suspended_at is NOT NULL — NULL is the ordinary active state, " +
			"which is what leaves every pre-existing row correct untouched")
	}
}

// TestMigration0024_UpDefaultsAllowRidesClosed pins the fail-closed default: a
// row inserted by a path that forgets the column grants the SMALLER thing.
func TestMigration0024_UpDefaultsAllowRidesClosed(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanVehicleShares(t)
	ctx := context.Background()

	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_vehicle_shares (id, vehicle_id, owner_user_id, label, permission, code, expires_at)
		 VALUES ('cmig0024a','cveh0024','cown0024','L','live','AAA111', NOW() + INTERVAL '7 days')`,
	); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var allowRides bool
	var suspendedAt *string
	if err := testPool.QueryRow(ctx,
		`SELECT allow_rides, suspended_at::text FROM go_vehicle_shares WHERE id = 'cmig0024a'`,
	).Scan(&allowRides, &suspendedAt); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if allowRides {
		t.Error("allow_rides defaulted to TRUE — the default must be the smaller capability")
	}
	if suspendedAt != nil {
		t.Error("suspended_at defaulted to non-NULL — a new grant must never be born paused")
	}
}

// TestMigration0024_UpBackfillsFromThePermissionTier is the migration's real
// job: every grant that already existed must keep conveying what it was sold at.
//
// The 'live_history' row is the one that matters. It collapses onto the SAME
// flags as 'live', which is not an approximation — it is the product decision
// this migration lands. That grant loses the drives surfaces (removed from the
// product) and NOTHING ELSE.
//
// THIS TEST RUNS THE MIGRATION FILE, NOT A COPY OF IT. It rolls the schema back
// to 23, seeds rows at the genuine pre-0024 shape — which is a shape with no
// allow_rides column to pre-set, so the tier is the only input, exactly as it
// was for the production rows — and then migrates UP, letting golang-migrate
// execute 0024_vehicle_share_grant_flags.up.sql itself. The earlier form of this
// test retyped the backfill UPDATE inline and asserted against that, which meant
// the one statement in this change that mutates existing production rows had no
// coverage at all: editing the .sql file could not fail it, and a typo divergence
// between the file and the test would have read as a passing test. A down/up
// cycle is not "a test of golang-migrate" — golang-migrate is the thing that
// will run this file in production, so it is precisely the path worth exercising.
func TestMigration0024_UpBackfillsFromThePermissionTier(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanVehicleShares(t)
	ctx := context.Background()

	m := newTestMigrator(t)
	defer func() { _, _ = m.Close() }()

	if err := m.Migrate(23); err != nil {
		t.Fatalf("migrate down to 23: %v", err)
	}
	// Restore the schema no matter how the assertions below go, so whatever
	// runs next still sees a head database.
	t.Cleanup(func() {
		if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
			t.Fatalf("restore migrations to head: %v", err)
		}
	})

	// Seeded at the PRE-MIGRATION SHAPE: no allow_rides column exists yet, so
	// there is nothing to pre-set and `permission` is the sole input to the
	// mapping — the same starting position every row in production was in.
	//
	// The pending and revoked rows are here because the backfill deliberately
	// runs over EVERY status, not just 'accepted': a pending row's flags must
	// not disagree with the grant it becomes, and a revoked tombstone must keep
	// an accurate record of what it conveyed while it was live.
	seed := `INSERT INTO go_vehicle_shares
		(id, vehicle_id, owner_user_id, label, permission, code, status, expires_at)
		VALUES ($1,$2,'cown0024','L',$3,$4,$5, NOW() + INTERVAL '7 days')`
	for _, row := range []struct{ id, vehicle, permission, code, status string }{
		{"cmig0024live", "cveh0024a", "live", "BBB111", "accepted"},
		{"cmig0024hist", "cveh0024b", "live_history", "BBB222", "accepted"},
		{"cmig0024ride", "cveh0024c", "rides", "BBB333", "accepted"},
		{"cmig0024pend", "cveh0024d", "rides", "BBB444", "pending"},
		{"cmig0024revk", "cveh0024e", "rides", "BBB555", "revoked"},
	} {
		if _, err := testPool.Exec(ctx, seed, row.id, row.vehicle, row.permission, row.code, row.status); err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}

	// THE MIGRATION ITSELF — ALTER plus the backfill UPDATE, read from
	// migrations/0024_vehicle_share_grant_flags.up.sql on disk.
	if err := m.Migrate(24); err != nil {
		t.Fatalf("migrate up to 24: %v", err)
	}

	want := map[string]bool{
		"cmig0024live": false,
		// THE RETIREMENT: live_history maps to the same flags as live.
		"cmig0024hist": false,
		"cmig0024ride": true,
		// Every status, not just accepted.
		"cmig0024pend": true,
		"cmig0024revk": true,
	}
	for id, wantRides := range want {
		var got bool
		var suspendedAt *string
		if err := testPool.QueryRow(ctx,
			`SELECT allow_rides, suspended_at::text FROM go_vehicle_shares WHERE id = $1`, id,
		).Scan(&got, &suspendedAt); err != nil {
			t.Fatalf("read %s: %v", id, err)
		}
		if got != wantRides {
			t.Errorf("%s: allow_rides = %v, want %v", id, got, wantRides)
		}
		// The backfill must not suspend anything. A migration that paused
		// every existing grant would be an outage, not a schema change.
		if suspendedAt != nil {
			t.Errorf("%s: the backfill set suspended_at = %v — no pre-existing grant may be paused by a migration", id, *suspendedAt)
		}
	}
}

// TestMigration0024_UpKeepsThePermissionCheckPermissive pins that the CHECK
// constraint still admits 'live_history'. Existing rows carry it, and
// tightening a CHECK under rows that violate it fails the migration outright —
// the value is simply never written again.
func TestMigration0024_UpKeepsThePermissionCheckPermissive(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanVehicleShares(t)

	if _, err := testPool.Exec(context.Background(),
		`INSERT INTO go_vehicle_shares (id, vehicle_id, owner_user_id, label, permission, code, expires_at)
		 VALUES ('cmig0024lh','cveh0024','cown0024','L','live_history','CCC111', NOW() + INTERVAL '7 days')`,
	); err != nil {
		t.Fatalf("the retired tier must remain insertable at the DB level: %v", err)
	}
}

// TestMigration0024_DownDropsOnlyTheFlags exercises the rollback and proves it
// is SURGICAL: the two 0024 columns go and everything migration 0020 installed
// — including `permission`, which is what the pre-MYR-369 gates fall back to —
// survives.
//
// Migrates down TO VERSION 23 and back up, leaving the database at head.
// Version-targeted rather than Steps(-1) on purpose: a relative step silently
// rolls back whatever the newest migration happens to be, so the next issue that
// adds one would turn this into a test of ITS down-migration.
func TestMigration0024_DownDropsOnlyTheFlags(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)

	m := newTestMigrator(t)
	defer func() { _, _ = m.Close() }()

	if err := m.Migrate(23); err != nil {
		t.Fatalf("migrate down to 23: %v", err)
	}
	// Restore the schema no matter how the assertions below go, so whatever
	// runs next still sees a head database.
	t.Cleanup(func() {
		if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
			t.Fatalf("restore migrations to head: %v", err)
		}
	})

	got := vehicleShareColumnTypes(t)
	for col := range migration0024Columns {
		if _, present := got[col]; present {
			t.Errorf("column %s survived the down-migration", col)
		}
	}

	// The rollback must not take the rest of the sharing schema with it. In
	// particular `permission` — the invite-time preset — is the ONLY record
	// of what each grant was sold at, and the pre-MYR-369 tier gates read it.
	// Dropping it would make the rollback unrecoverable.
	for _, survivor := range []string{"id", "vehicle_id", "owner_user_id", "label", "permission", "code", "status", "accepted_by_user_id", "revoked_at"} {
		if _, present := got[survivor]; !present {
			t.Errorf("column %s was dropped by the 0024 down-migration — it must be surgical", survivor)
		}
	}

	// And the surrounding Go-owned tables are untouched.
	for _, table := range []string{"go_vehicle_shares", "go_ride_requests", "go_users", "go_saved_places"} {
		if !tableExists(t, table) {
			t.Errorf("table %s was dropped by the 0024 down-migration", table)
		}
	}
}
