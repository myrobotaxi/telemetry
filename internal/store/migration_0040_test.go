package store_test

import (
	"context"
	"testing"
)

// MYR-540 — migration 0040's shape. Four things about the group-ride schema are
// load-bearing enough that a silent change to any of them would be an
// access-control or data-loss defect rather than a slowdown: the code's
// UNIQUENESS (the by-code lookup is ambiguous without it), the membership key
// (the join's idempotence rests on it), the CASCADE (the retention sweep cannot
// strand a standing grant), and the user index (the access set resolves through
// it on every handshake).

// TestMigration0040_GroupRideColumns pins the three columns on the ride row and
// the partial unique index over the code.
func TestMigration0040_GroupRideColumns(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	for _, col := range []string{"group_ride", "join_code", "join_code_expires_at"} {
		var n int
		if err := testPool.QueryRow(ctx,
			`SELECT count(*) FROM information_schema.columns
			  WHERE table_name = 'go_ride_requests' AND column_name = $1`, col,
		).Scan(&n); err != nil {
			t.Fatalf("read column %s: %v", col, err)
		}
		if n == 0 {
			t.Errorf("go_ride_requests.%s missing after migrate up", col)
		}
	}

	// ABSENT MEANS FALSE is the contract's rule for `groupRide`, and this
	// default is what makes every pre-MYR-540 row satisfy it without a rewrite.
	var groupDefault, notNull string
	if err := testPool.QueryRow(ctx,
		`SELECT COALESCE(column_default, ''), is_nullable FROM information_schema.columns
		  WHERE table_name = 'go_ride_requests' AND column_name = 'group_ride'`,
	).Scan(&groupDefault, &notNull); err != nil {
		t.Fatalf("read group_ride default: %v", err)
	}
	if groupDefault != "false" || notNull != "NO" {
		t.Errorf("group_ride is (default %q, nullable %q), want (\"false\", \"NO\") — "+
			"a nullable flag would make absence mean \"unknown\"", groupDefault, notNull)
	}

	var idx int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes
		  WHERE tablename = 'go_ride_requests' AND indexname = 'uq_go_ride_requests_join_code'`,
	).Scan(&idx); err != nil {
		t.Fatalf("read join-code index: %v", err)
	}
	if idx == 0 {
		t.Fatal("uq_go_ride_requests_join_code missing; the by-code lookup would be ambiguous")
	}
}

// TestMigration0040_JoinCodeIsUnique proves the index actually refuses a
// duplicate — the property the mint's collision retry is catching, and the one
// that makes "resolve a code to A ride" a well-defined operation.
func TestMigration0040_JoinCodeIsUnique(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	seedGroupRide(ctx, t, "cride0540uqa", "crider0540a")
	seedGroupRide(ctx, t, "cride0540uqb", "crider0540b")

	if _, err := testPool.Exec(ctx,
		`UPDATE go_ride_requests SET join_code = 'RBO246' WHERE id = 'cride0540uqa'`); err != nil {
		t.Fatalf("stamp first code: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`UPDATE go_ride_requests SET join_code = 'RBO246' WHERE id = 'cride0540uqb'`); err == nil {
		t.Fatal("a second ride took the same join code; the redeem lookup would be ambiguous")
	}

	// NULL is the ordinary state of the column and must NOT collide — the index
	// is partial precisely so every solo ride can carry NULL.
	var nulls int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM go_ride_requests WHERE join_code IS NULL`).Scan(&nulls); err != nil {
		t.Fatalf("count null codes: %v", err)
	}
	if nulls == 0 {
		t.Error("no rows with a NULL join_code; the partial index's whole point is untested")
	}
}

// TestMigration0040_MembersShape pins the composite key the join's idempotence
// rests on and the user index the access set resolves through.
func TestMigration0040_MembersShape(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	seedGroupRide(ctx, t, "cride0540key", "crider0540key")
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_ride_members (ride_id, user_id) VALUES ('cride0540key', 'cjoiner0540')`); err != nil {
		t.Fatalf("seed member: %v", err)
	}
	// The second insert is what ON CONFLICT DO NOTHING relies on. Without the
	// key it would succeed and the ride would carry the same person twice.
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_ride_members (ride_id, user_id) VALUES ('cride0540key', 'cjoiner0540')`); err == nil {
		t.Fatal("a duplicate membership inserted; the join would not be idempotent")
	}

	var idx int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM pg_indexes
		  WHERE tablename = 'go_ride_members' AND indexname = 'idx_go_ride_members_user'`,
	).Scan(&idx); err != nil {
		t.Fatalf("read member index: %v", err)
	}
	if idx == 0 {
		t.Error("idx_go_ride_members_user missing; every WS handshake would seq-scan the table")
	}
}

// TestMigration0040_MembersCascadeWithTheirRide is the retention argument, and
// it is why the FK exists at all. The MYR-447 pruner DELETEs terminal ride rows
// directly; a membership row that survived would be a standing grant on somebody
// else's vehicle with nothing left to revoke it.
func TestMigration0040_MembersCascadeWithTheirRide(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()

	const rideID = "cride0540cascade"
	seedGroupRide(ctx, t, rideID, "crider0540c")
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_ride_members (ride_id, user_id) VALUES ($1, 'cjoiner0540c')`, rideID); err != nil {
		t.Fatalf("seed member: %v", err)
	}

	if _, err := testPool.Exec(ctx, `DELETE FROM go_ride_requests WHERE id = $1`, rideID); err != nil {
		t.Fatalf("delete ride: %v", err)
	}
	var left int
	if err := testPool.QueryRow(ctx,
		`SELECT count(*) FROM go_ride_members WHERE ride_id = $1`, rideID).Scan(&left); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if left != 0 {
		t.Errorf("%d membership row(s) outlived their ride; each is a standing grant nothing can revoke", left)
	}
}

// seedGroupRide inserts a minimal accepted GROUP ride. The coordinate columns
// take the literal 'enc' the other migration tests use — nothing here decrypts.
func seedGroupRide(ctx context.Context, t *testing.T, rideID, riderID string) {
	t.Helper()
	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_ride_requests (
			id, rider_id, owner_id, vehicle_id,
			pickup_lat_enc, pickup_lng_enc, pickup_label,
			dropoff_lat_enc, dropoff_lng_enc, dropoff_label, status, group_ride
		) VALUES ($1, $2, 'cowner0540', 'cveh0540',
			'enc', 'enc', 'Home', 'enc', 'enc', 'Work', 'accepted', TRUE)`,
		rideID, riderID); err != nil {
		t.Fatalf("seed group ride %s: %v", rideID, err)
	}
}
