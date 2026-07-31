package store_test

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-398 migration 0027: the Live Activity leg-progress anchor.
//
// Four nullable columns on go_live_activities that remember what a progress
// fraction is a fraction OF. The column TYPES and the undocumented-column sweep
// live with the rest of the table's shape in migration_0025_test.go; what is
// asserted here is everything the fraction's honesty depends on.

// TestMigration0027_AnchorColumnsAreBornEmpty is the "no default" rule.
//
// A DEFAULT of 0 would have been a lie with a number on it — "the car has
// covered none of the distance" is a claim, and "we do not know where the car
// is" is the truth about a row nobody has pushed to yet. The sender renders the
// second as no track at all.
func TestMigration0027_AnchorColumnsAreBornEmpty(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanGoLiveActivities(t)
	seedActivityRide(t, "cride0025p1")
	ctx := context.Background()

	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_live_activities (id, ride_request_id, user_id, activity_push_token)
		 VALUES ('cact0027a', 'cride0025p1', 'crider0027', 'token-progress')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var leg, source *string
	var baseline, value *float64
	if err := testPool.QueryRow(ctx,
		`SELECT progress_leg, progress_source, progress_baseline, progress_value
		 FROM go_live_activities WHERE id = 'cact0027a'`,
	).Scan(&leg, &source, &baseline, &value); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if leg != nil || source != nil || baseline != nil || value != nil {
		t.Errorf("a fresh registration carries an anchor (%v, %v, %v, %v); it must be born empty",
			leg, source, baseline, value)
	}

	nullable := liveActivityNullability(t)
	for _, col := range []string{"progress_leg", "progress_source", "progress_baseline", "progress_value"} {
		if nullable[col] != "YES" {
			t.Errorf("%s is NOT NULL; absent progress is the ordinary state and must be representable", col)
		}
	}
}

// TestMigration0027_ChecksRefuseNonsense pins the three CHECK constraints. Each
// one guards a value the sender can never produce and a bad backfill could.
func TestMigration0027_ChecksRefuseNonsense(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanGoLiveActivities(t)
	seedActivityRide(t, "cride0025p2")
	ctx := context.Background()

	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_live_activities (id, ride_request_id, user_id, activity_push_token)
		 VALUES ('cact0027b', 'cride0025p2', 'crider0027b', 'token-checks')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	for _, tc := range []struct {
		name string
		set  string
	}{
		{"a leg that is not one of the two", `progress_leg = 'somewhere'`},
		{"a source that names no unit", `progress_source = 'vibes'`},
		{"a fraction above one", `progress_value = 1.5`},
		{"a negative fraction", `progress_value = -0.1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := testPool.Exec(ctx,
				`UPDATE go_live_activities SET `+tc.set+` WHERE id = 'cact0027b'`)
			if err == nil {
				t.Errorf("the database accepted %s", tc.set)
			}
		})
	}
}

// TestMigration0027_TokenRotationKeepsTheAnchor is the interaction with 0025's
// upsert, and the reason it is worth a test rather than a comment.
//
// ActivityKit rotates the update token mid-Activity and the server re-registers
// against the same (ride, party) row. That is the SAME Activity on the SAME
// leg: if the rotation reset the anchor, the rider would watch the arrow snap
// back to the start of the journey for a reason no log line would ever explain.
func TestMigration0027_TokenRotationKeepsTheAnchor(t *testing.T) {
	const ride = "cride0025p3"
	repo := setupLiveActivities(t, ride)
	ctx := context.Background()

	if err := repo.RegisterActivity(ctx, ride, "crider0027c", "token-first", false); err != nil {
		t.Fatalf("register: %v", err)
	}
	key := store.LiveActivityKey{RideRequestID: ride, UserID: "crider0027c"}
	anchor := store.LiveActivityProgress{Leg: "pickup", Source: "nav_distance", Baseline: 12, Value: 0.5}
	if err := repo.SaveActivityProgress(ctx, key, anchor); err != nil {
		t.Fatalf("SaveActivityProgress: %v", err)
	}

	if err := repo.RegisterActivity(ctx, ride, "crider0027c", "token-rotated", false); err != nil {
		t.Fatalf("re-register (rotation): %v", err)
	}

	got, err := repo.ActivitiesForRide(ctx, ride)
	if err != nil {
		t.Fatalf("ActivitiesForRide: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("activities = %d, want 1", len(got))
	}
	if got[0].ActivityPushToken != "token-rotated" {
		t.Errorf("token = %q, want the rotated one", got[0].ActivityPushToken)
	}
	if got[0].Progress != anchor {
		t.Errorf("anchor = %+v after a token rotation, want %+v", got[0].Progress, anchor)
	}
}

// TestMigration0027_DownDropsOnlyTheAnchor rolls back through the real
// golang-migrate files. A server rolled back past 0027 must lose the track and
// nothing else: `progress` is optional on the wire, so the card degrades to the
// headline-only rendering an older app build gets anyway.
func TestMigration0027_DownDropsOnlyTheAnchor(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanGoLiveActivities(t)

	m := newTestMigrator(t)
	defer func() { _, _ = m.Close() }()

	t.Cleanup(func() {
		if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
			t.Fatalf("restore migrations to head: %v", err)
		}
	})

	if err := m.Migrate(26); err != nil {
		t.Fatalf("migrate down to 26: %v", err)
	}
	cols := liveActivityColumnTypes(t)
	for _, col := range []string{"progress_leg", "progress_source", "progress_baseline", "progress_value"} {
		if _, present := cols[col]; present {
			t.Errorf("%s survived the down-migration", col)
		}
	}
	// Surgical: the registry itself, and the token it addresses an Activity by,
	// are 0025's and must be untouched.
	if _, present := cols["activity_push_token"]; !present {
		t.Fatal("the down-migration took the registry's own columns with it")
	}

	if err := m.Migrate(27); err != nil {
		t.Fatalf("migrate back up to 27: %v", err)
	}
	cols = liveActivityColumnTypes(t)
	for _, col := range []string{"progress_leg", "progress_source", "progress_baseline", "progress_value"} {
		if _, present := cols[col]; !present {
			t.Errorf("%s missing after re-applying the up-migration", col)
		}
	}
}
