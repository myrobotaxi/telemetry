package store_test

import (
	"context"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-398 migration 0029: the Live Activity island auto-expand high-water mark.
//
// One SMALLINT on go_live_activities recording the highest phase this Activity
// has already been expanded for. Everything asserted here is about a rule that
// lives in SQL — a CHECK, an INSERT-time CASE, a guarded UPDATE — so a fake
// would prove nothing.

// alertedPhaseOf reads one row's mark.
func alertedPhaseOf(t *testing.T, activityID string) int16 {
	t.Helper()
	var phase int16
	if err := testPool.QueryRow(context.Background(),
		`SELECT alerted_phase FROM go_live_activities WHERE id = $1`, activityID).Scan(&phase); err != nil {
		t.Fatalf("read alerted_phase for %s: %v", activityID, err)
	}
	return phase
}

// alertedPhaseFor reads the mark by natural key, which is what the repo writes
// against.
func alertedPhaseFor(t *testing.T, rideID, userID string) int16 {
	t.Helper()
	var phase int16
	if err := testPool.QueryRow(context.Background(),
		`SELECT alerted_phase FROM go_live_activities
		  WHERE ride_request_id = $1 AND user_id = $2`, rideID, userID).Scan(&phase); err != nil {
		t.Fatalf("read alerted_phase for (%s, %s): %v", rideID, userID, err)
	}
	return phase
}

// TestMigration0029_MarkIsNotNullAndBoundedToTheLadder.
//
// Unlike the progress anchor beside it, there is no honest-unknown state here:
// an Activity has either been alerted at some phase or it has not, and 0 says
// the second exactly. So this column is NOT NULL where those are nullable, and
// the CHECK pins it to the ladder's own range.
func TestMigration0029_MarkIsNotNullAndBoundedToTheLadder(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanGoLiveActivities(t)
	seedActivityRide(t, "cride0029a")
	ctx := context.Background()

	if nullable := liveActivityNullability(t); nullable["alerted_phase"] != "NO" {
		t.Errorf("alerted_phase is nullable; there is no unknown state for it to represent")
	}

	if _, err := testPool.Exec(ctx,
		`INSERT INTO go_live_activities (id, ride_request_id, user_id, activity_push_token)
		 VALUES ('cact0029a', 'cride0029a', 'crider0029', 'token-alert')`); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A RAW insert takes the column DEFAULT, which is 0. The phase seeding is
	// the repo's upsert doing it, not the column — asserted separately below,
	// and worth keeping apart: the default has to be the safe value for any
	// writer that does not know about the ladder, and 0 ("never alerted") is
	// that value, because it can only ever cost one extra expansion.
	if got := alertedPhaseOf(t, "cact0029a"); got != 0 {
		t.Errorf("alerted_phase = %d on a raw insert, want the column default 0", got)
	}

	for _, bad := range []int{-1, 7} {
		if _, err := testPool.Exec(ctx,
			`UPDATE go_live_activities SET alerted_phase = $2 WHERE id = 'cact0029a'`, bad); err == nil {
			t.Errorf("alerted_phase = %d was accepted; the CHECK must pin it to the 0..6 ladder", bad)
		}
	}
}

// TestMigration0029_RegistrationSeedsTheRidesCurrentPhase.
//
// The app starts the Activity locally and draws the current state itself, so
// the island has just been in front of the rider. Seeding says "that phase has
// happened"; without it the very next push — within 75 seconds — would expand
// the island to announce a state the rider is already looking at.
//
// The gaps in the ladder are deliberate and are asserted: `arrived` seeds 4 and
// `enroute` seeds 5, skipping Arriving, because 3 is a threshold on the car's
// ETA that a status alone cannot express.
func TestMigration0029_RegistrationSeedsTheRidesCurrentPhase(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	ctx := context.Background()
	repo := store.NewLiveActivityRepo(testPool, nil)

	tests := []struct {
		status string
		want   int16
	}{
		{"requested", 1},
		{"accepted", 2},
		{"arrived", 4},
		{"enroute", 5},
	}

	for _, tc := range tests {
		t.Run(tc.status, func(t *testing.T) {
			cleanGoLiveActivities(t)
			rideID := "cride0029s" + tc.status[:1]
			seedActivityRide(t, rideID)
			setRideStatus(t, rideID, tc.status)

			if err := repo.RegisterActivity(ctx, rideID, "crider0029s", "abcdef01", false); err != nil {
				t.Fatalf("register on a %s ride: %v", tc.status, err)
			}
			if got := alertedPhaseFor(t, rideID, "crider0029s"); got != tc.want {
				t.Errorf("alerted_phase = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestMigration0029_TokenRotationPreservesTheMark.
//
// ActivityKit rotates the push token mid-ride and expects the server to switch
// to the new one, so a rotation is an ordinary re-registration — of the SAME
// Activity, mid-ladder. Re-running the whole ladder because the token changed
// would open the island once for every phase the ride has already passed.
func TestMigration0029_TokenRotationPreservesTheMark(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanGoLiveActivities(t)
	seedActivityRide(t, "cride0029r")
	ctx := context.Background()
	repo := store.NewLiveActivityRepo(testPool, nil)

	if err := repo.RegisterActivity(ctx, "cride0029r", "crider0029r", "abcdef01", false); err != nil {
		t.Fatalf("register: %v", err)
	}
	// The ride progresses and the island is expanded for On trip.
	if err := repo.SaveActivityAlertedPhase(ctx,
		store.LiveActivityKey{RideRequestID: "cride0029r", UserID: "crider0029r"}, 5); err != nil {
		t.Fatalf("save mark: %v", err)
	}

	if err := repo.RegisterActivity(ctx, "cride0029r", "crider0029r", "beefcafe", false); err != nil {
		t.Fatalf("rotate token: %v", err)
	}
	if got := alertedPhaseFor(t, "cride0029r", "crider0029r"); got != 5 {
		t.Errorf("alerted_phase = %d after a token rotation, want the preserved 5", got)
	}
}

// TestMigration0029_SaveRefusesToLowerTheMark is the unsharded-ticker guard.
//
// Every replica lists every live Activity, so two of them can read the same
// mark seconds apart and race to write it back. If the one that computed the
// LOWER phase won, the higher phase would alert a second time — the island
// opening twice for one state change, which is the exact failure this column
// exists to prevent.
func TestMigration0029_SaveRefusesToLowerTheMark(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanGoLiveActivities(t)
	seedActivityRide(t, "cride0029g")
	ctx := context.Background()
	repo := store.NewLiveActivityRepo(testPool, nil)
	key := store.LiveActivityKey{RideRequestID: "cride0029g", UserID: "crider0029g"}

	if err := repo.RegisterActivity(ctx, "cride0029g", "crider0029g", "abcdef01", false); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := repo.SaveActivityAlertedPhase(ctx, key, 4); err != nil {
		t.Fatalf("save 4: %v", err)
	}

	// A losing replica writes back an earlier phase. Not an error — it is an
	// ordinary race — but it must not land.
	if err := repo.SaveActivityAlertedPhase(ctx, key, 2); err != nil {
		t.Fatalf("save 2 returned an error; a losing write is a silent no-op: %v", err)
	}
	if got := alertedPhaseFor(t, "cride0029g", "crider0029g"); got != 4 {
		t.Errorf("alerted_phase = %d, want the higher 4 to have held", got)
	}

	// Forward still works.
	if err := repo.SaveActivityAlertedPhase(ctx, key, 5); err != nil {
		t.Fatalf("save 5: %v", err)
	}
	if got := alertedPhaseFor(t, "cride0029g", "crider0029g"); got != 5 {
		t.Errorf("alerted_phase = %d, want 5", got)
	}

	// And a non-positive phase is a caller bug rather than a no-op: 0 is a
	// starting value, never something to record.
	if err := repo.SaveActivityAlertedPhase(ctx, key, 0); err == nil {
		t.Error("SaveActivityAlertedPhase(0) succeeded; zero means never alerted and is not writable")
	}
}

// TestMigration0029_SaveSkipsATombstonedRow. A write racing the ride's terminal
// end must not resurrect rendering state onto an Activity nothing will push to
// again — the same scoping every other write on this table takes.
func TestMigration0029_SaveSkipsATombstonedRow(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanGoLiveActivities(t)
	seedActivityRide(t, "cride0029t")
	ctx := context.Background()
	repo := store.NewLiveActivityRepo(testPool, nil)
	key := store.LiveActivityKey{RideRequestID: "cride0029t", UserID: "crider0029t"}

	if err := repo.RegisterActivity(ctx, "cride0029t", "crider0029t", "abcdef01", false); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := repo.EndActivitiesForRide(ctx, "cride0029t"); err != nil {
		t.Fatalf("tombstone: %v", err)
	}

	if err := repo.SaveActivityAlertedPhase(ctx, key, 6); err != nil {
		t.Fatalf("save against a tombstoned row must be a silent no-op, not an error: %v", err)
	}
	// Still the registration seed (2, `accepted`), untouched.
	if got := alertedPhaseFor(t, "cride0029t", "crider0029t"); got != 2 {
		t.Errorf("alerted_phase = %d on a tombstoned row, want the untouched 2", got)
	}
}

// TestMigration0029_ReadPathsCarryTheMark. Both send paths clamp against it, so
// a projection that forgot to select it would silently re-run the whole ladder
// on every push — the strobing island, arrived at by omission.
func TestMigration0029_ReadPathsCarryTheMark(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanGoLiveActivities(t)
	seedActivityRide(t, "cride0029p")
	ctx := context.Background()
	repo := store.NewLiveActivityRepo(testPool, nil)
	key := store.LiveActivityKey{RideRequestID: "cride0029p", UserID: "crider0029p"}

	if err := repo.RegisterActivity(ctx, "cride0029p", "crider0029p", "abcdef01", false); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := repo.SaveActivityAlertedPhase(ctx, key, 4); err != nil {
		t.Fatalf("save mark: %v", err)
	}

	rows, err := repo.ActivitiesForRide(ctx, "cride0029p")
	if err != nil {
		t.Fatalf("ActivitiesForRide: %v", err)
	}
	if len(rows) != 1 || rows[0].AlertedPhase != 4 {
		t.Errorf("lifecycle fan-out read %d rows with mark %v, want one row at 4", len(rows), rows)
	}

	legs, err := repo.ListActiveLegActivities(ctx, 10)
	if err != nil {
		t.Fatalf("ListActiveLegActivities: %v", err)
	}
	if len(legs) != 1 || legs[0].AlertedPhase != 4 {
		t.Errorf("ticker read %d legs with mark %v, want one leg at 4", len(legs), legs)
	}
}

// TestMigration0029_DownDropsOnlyTheMark rolls back through the real
// golang-migrate files.
//
// A server rolled back past 0029 stops attaching alert dictionaries and the
// islands stop expanding; the cards themselves keep updating exactly as before,
// because the mark is rendering memory rather than a fact about a ride.
//
// The roll FORWARD is the part worth pinning, and it is why this test goes back
// up rather than stopping at the drop: the re-applied migration's backfill
// re-seeds the marks from ride status, so an Activity mid-pickup gets at most
// one more expansion than it strictly earned. One extra island opening on a
// subset of in-flight rides is the whole cost of this being reversible, and the
// assertion below is that the column and its seeding both come back.
func TestMigration0029_DownDropsOnlyTheMark(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanGoLiveActivities(t)
	seedActivityRide(t, "cride0029d")
	ctx := context.Background()
	repo := store.NewLiveActivityRepo(testPool, nil)

	if err := repo.RegisterActivity(ctx, "cride0029d", "crider0029d", "abcdef01", false); err != nil {
		t.Fatalf("register: %v", err)
	}

	m := newTestMigrator(t)
	defer func() { _, _ = m.Close() }()

	t.Cleanup(func() {
		if err := store.RunMigrations(context.Background(), testConnStr, testLogger()); err != nil {
			t.Fatalf("restore migrations to head: %v", err)
		}
	})

	// Version-targeted, not Steps(-1): a relative step would silently become a
	// test of whatever migration lands next.
	if err := m.Migrate(28); err != nil {
		t.Fatalf("migrate down to 28: %v", err)
	}
	cols := liveActivityColumnTypes(t)
	if _, present := cols["alerted_phase"]; present {
		t.Error("alerted_phase survived the down-migration")
	}
	// Surgical: 0025's registry and 0027's anchor must be untouched.
	if _, present := cols["activity_push_token"]; !present {
		t.Fatal("the down-migration took the registry's own columns with it")
	}
	for _, col := range progress0027Columns {
		if _, present := cols[col]; !present {
			t.Errorf("the 0029 rollback dropped 0027's %s; the down must be surgical", col)
		}
	}

	if err := m.Migrate(29); err != nil {
		t.Fatalf("migrate back up to 29: %v", err)
	}
	if _, present := liveActivityColumnTypes(t)["alerted_phase"]; !present {
		t.Fatal("alerted_phase missing after re-applying the up-migration")
	}
	// The backfill ran on the way back up: the surviving row's ride is
	// `accepted`, so its mark is re-seeded at Enroute rather than left at the
	// column default. That is what stops a roll-forward from opening every live
	// island at once.
	if got := alertedPhaseFor(t, "cride0029d", "crider0029d"); got != 2 {
		t.Errorf("alerted_phase = %d after re-applying, want the backfilled 2 (Enroute)", got)
	}
}

// TestMigration0029_TickerListsARequestedRide is the Dispatch-phase widening
// (MYR-398): the v3 card starts the Activity at REQUEST, so a ride awaiting the
// owner's answer must keep receiving pushes.
//
// Without it the Dispatch card has its stale-date expire three minutes in, and
// "Finding your ride" starts apologising for being out of date while the search
// is genuinely still running.
func TestMigration0029_TickerListsARequestedRide(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping migration integration test")
	}
	mustApplyGoMigrations(t)
	cleanGoLiveActivities(t)
	seedActivityRide(t, "cride0029q")
	setRideStatus(t, "cride0029q", "requested")
	ctx := context.Background()
	repo := store.NewLiveActivityRepo(testPool, nil)

	if err := repo.RegisterActivity(ctx, "cride0029q", "crider0029q", "abcdef01", false); err != nil {
		t.Fatalf("registration on a `requested` ride was refused: %v", err)
	}

	legs, err := repo.ListActiveLegActivities(ctx, 10)
	if err != nil {
		t.Fatalf("ListActiveLegActivities: %v", err)
	}
	if len(legs) != 1 {
		t.Fatalf("ticker listed %d legs for a `requested` ride, want 1", len(legs))
	}
	if got := legs[0].Status; got != store.RideRequestStatusRequested {
		t.Errorf("status = %s, want requested", got)
	}
	// Seeded at Dispatch, so the accept transition is the first expansion.
	if legs[0].AlertedPhase != 1 {
		t.Errorf("alerted_phase = %d, want 1 (Dispatch)", legs[0].AlertedPhase)
	}
}
