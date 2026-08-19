package store_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-592 — the two Go-owned tables behind owner-inactivity telemetry
// suspension, exercised against a real Postgres.
//
// The arms that matter are the ones whose failure is silent in production: a
// throttle that does not throttle (the busiest write in the database), a
// candidate query that treats "never observed" as "infinitely idle" (the entire
// fleet suspended on the first pass after deploy), and an episode mark that
// moves an instant the catalog already emitted.

const suspensionThrottle = time.Hour

func suspensionNow() time.Time { return time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC) }

// cleanSuspensionTables empties the three Go-owned tables this feature reads.
// The shared cleanTables helper only reaches the Prisma-owned pair, and a
// surviving go_user_activity row from a previous case turns "never observed"
// into "observed", which is precisely the arm this file has to be able to prove.
func cleanSuspensionTables(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	for _, table := range []string{
		"go_user_activity",
		"go_vehicle_telemetry_suspensions",
		"go_fleet_config_attempts",
	} {
		if _, err := testPool.Exec(ctx, "DELETE FROM "+table); err != nil {
			t.Fatalf("clean %s: %v", table, err)
		}
	}
}

// seedActivity writes a last-seen row directly, bypassing the throttle, so a
// test can place an owner at an arbitrary distance in the past.
func seedActivity(t *testing.T, userID string, at time.Time) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`INSERT INTO go_user_activity (user_id, last_seen_at) VALUES ($1, $2)
		 ON CONFLICT (user_id) DO UPDATE SET last_seen_at = EXCLUDED.last_seen_at`,
		userID, at)
	if err != nil {
		t.Fatalf("seed activity for %s: %v", userID, err)
	}
}

func seedFleetConfigOutcome(t *testing.T, vehicleID, outcome string) {
	t.Helper()
	_, err := testPool.Exec(context.Background(),
		`INSERT INTO go_fleet_config_attempts (vehicle_id, last_outcome) VALUES ($1, $2)
		 ON CONFLICT (vehicle_id) DO UPDATE SET last_outcome = EXCLUDED.last_outcome`,
		vehicleID, outcome)
	if err != nil {
		t.Fatalf("seed fleet config outcome for %s: %v", vehicleID, err)
	}
}

func TestUserActivityRepo_TouchThrottles(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	cleanTables(t, testPool)
	cleanSuspensionTables(t)

	repo := store.NewUserActivityRepo(testPool)
	ctx := context.Background()
	base := suspensionNow()

	tests := []struct {
		name      string
		at        time.Time
		wantWrote bool
		reason    string
	}{
		{
			name:      "the first sighting inserts",
			at:        base,
			wantWrote: true,
			reason:    "an account with no row has never been observed; the INSERT arm is unconditional on purpose",
		},
		{
			name:      "a second request one second later does not move the row",
			at:        base.Add(time.Second),
			wantWrote: false,
			reason:    "this is the guard the hot path depends on — a WS client revalidates continuously",
		},
		{
			name:      "still throttled a minute before the window closes",
			at:        base.Add(suspensionThrottle - time.Minute),
			wantWrote: false,
			reason:    "the predicate compares the STORED value, not EXCLUDED",
		},
		{
			name:      "the window reopens once it has elapsed",
			at:        base.Add(suspensionThrottle + time.Second),
			wantWrote: true,
			reason:    "an account still active an hour later must not age into the sweeper",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wrote, err := repo.Touch(ctx, "user_throttle", tc.at, suspensionThrottle)
			if err != nil {
				t.Fatalf("Touch: %v", err)
			}
			if wrote != tc.wantWrote {
				t.Errorf("Touch wrote = %v, want %v (%s)", wrote, tc.wantWrote, tc.reason)
			}
		})
	}

	// The stored value is the last instant that actually won, not the last one
	// attempted.
	seen, ok, err := repo.LastSeen(ctx, "user_throttle")
	if err != nil {
		t.Fatalf("LastSeen: %v", err)
	}
	if !ok {
		t.Fatal("LastSeen reports no row after four Touches")
	}
	if want := base.Add(suspensionThrottle + time.Second); !seen.Equal(want) {
		t.Errorf("stored last_seen_at = %v, want %v", seen.UTC(), want)
	}
}

func TestUserActivityRepo_UnknownAccountAndDelete(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	cleanTables(t, testPool)
	cleanSuspensionTables(t)

	repo := store.NewUserActivityRepo(testPool)
	ctx := context.Background()

	// "Never observed" must be distinguishable from "observed long ago" — the
	// sweeper's whole safety argument rests on it.
	if _, ok, err := repo.LastSeen(ctx, "user_never"); err != nil || ok {
		t.Errorf("LastSeen(unknown) = (ok=%v, err=%v), want (false, nil)", ok, err)
	}

	if _, err := repo.Touch(ctx, "user_gone", suspensionNow(), suspensionThrottle); err != nil {
		t.Fatalf("Touch: %v", err)
	}
	// §7.6 step 8c. Idempotent, because the step may run for an account that
	// never authenticated after this table shipped.
	for i := range 2 {
		if err := repo.DeleteForUser(ctx, "user_gone"); err != nil {
			t.Fatalf("DeleteForUser (pass %d): %v", i, err)
		}
	}
	if _, ok, _ := repo.LastSeen(ctx, "user_gone"); ok {
		t.Error("row survived DeleteForUser")
	}
}

func TestTelemetrySuspensionRepo_CandidateArms(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)

	repo := store.NewTelemetrySuspensionRepo(testPool)
	ctx := context.Background()
	now := suspensionNow()
	warnBefore := now.Add(-4 * 24 * time.Hour)

	tests := []struct {
		name    string
		seed    func(t *testing.T, vehicleID, ownerID string)
		wantHit bool
		reason  string
	}{
		{
			name: "an owner silent for six days is a candidate",
			seed: func(t *testing.T, _, ownerID string) {
				seedActivity(t, ownerID, now.Add(-6*24*time.Hour))
			},
			wantHit: true,
			reason:  "this is the case the feature exists for",
		},
		{
			name: "an owner seen yesterday is not",
			seed: func(t *testing.T, _, ownerID string) {
				seedActivity(t, ownerID, now.Add(-24*time.Hour))
			},
			wantHit: false,
			reason:  "inside the warning threshold; nothing to say to them",
		},
		{
			name:    "an owner we have NEVER observed is not",
			seed:    func(_ *testing.T, _, _ string) {},
			wantHit: false,
			reason: "the INNER join is the safety property: a missing row means no evidence, " +
				"and an outer join would suspend the entire fleet on the first pass after deploy",
		},
		{
			name: "a car awaiting its virtual key is not",
			seed: func(t *testing.T, vehicleID, ownerID string) {
				seedActivity(t, ownerID, now.Add(-6*24*time.Hour))
				seedFleetConfigOutcome(t, vehicleID, store.SetupOutcomeAwaitingVirtualKey)
			},
			wantHit: false,
			reason:  "Tesla never installed a config, so there is nothing streaming and nothing to bill",
		},
		{
			name: "a car whose config push FAILED is not",
			seed: func(t *testing.T, vehicleID, ownerID string) {
				seedActivity(t, ownerID, now.Add(-6*24*time.Hour))
				seedFleetConfigOutcome(t, vehicleID, store.SetupOutcomePushFailed)
			},
			wantHit: false,
			reason:  "same reason — no config exists to remove",
		},
		{
			name: "a car whose link-time push APPLIED is a candidate",
			seed: func(t *testing.T, vehicleID, ownerID string) {
				seedActivity(t, ownerID, now.Add(-6*24*time.Hour))
				seedFleetConfigOutcome(t, vehicleID, store.SetupOutcomeNone)
			},
			wantHit: true,
			reason: "the empty outcome is what the hook seeds on the nil-error arm; " +
				"exempting it would disable the feature for the healthiest cars in the fleet",
		},
		{
			name: "an already-suspended car is not a candidate again",
			seed: func(t *testing.T, vehicleID, ownerID string) {
				seedActivity(t, ownerID, now.Add(-9*24*time.Hour))
				if err := repo.MarkSuspended(ctx, vehicleID, now.Add(-2*24*time.Hour)); err != nil {
					t.Fatalf("MarkSuspended: %v", err)
				}
			},
			wantHit: false,
			reason:  "there is no second action to take, and re-warning would be a lie",
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cleanTables(t, testPool)
			cleanSuspensionTables(t)
			vehicleID := fmt.Sprintf("veh_susp_%d", i)
			ownerID := fmt.Sprintf("user_susp_%d", i)
			seedVehicleForOwner(t, testPool, vehicleID, vinForIndex(600+i), ownerID)
			tc.seed(t, vehicleID, ownerID)

			got, err := repo.ListInactiveOwnerVehicles(ctx, warnBefore, 50)
			if err != nil {
				t.Fatalf("ListInactiveOwnerVehicles: %v", err)
			}
			if (len(got) == 1) != tc.wantHit {
				t.Fatalf("candidates = %d, want hit=%v (%s)", len(got), tc.wantHit, tc.reason)
			}
			if tc.wantHit {
				if got[0].VehicleID != vehicleID || got[0].OwnerID != ownerID {
					t.Errorf("candidate = (%s, %s), want (%s, %s)",
						got[0].VehicleID, got[0].OwnerID, vehicleID, ownerID)
				}
				if got[0].WarnedAt != nil {
					t.Error("a fresh episode reports a warning that was never sent")
				}
			}
		})
	}
}

// TestTelemetrySuspensionRepo_RiderActivityDoesNotDefer pins the explicit client
// ruling in SQL. Only the OWNER's activity counts.
func TestTelemetrySuspensionRepo_RiderActivityDoesNotDefer(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	cleanTables(t, testPool)
	cleanSuspensionTables(t)

	repo := store.NewTelemetrySuspensionRepo(testPool)
	ctx := context.Background()
	now := suspensionNow()

	seedVehicleForOwner(t, testPool, "veh_shared", vinForIndex(650), "user_owner_away")
	seedActivity(t, "user_owner_away", now.Add(-6*24*time.Hour))
	// A rider using the app every minute. The platform pays for the OWNER's
	// stream, and only the owner can decide to keep paying for it.
	seedActivity(t, "user_rider_active", now)

	got, err := repo.ListInactiveOwnerVehicles(ctx, now.Add(-4*24*time.Hour), 50)
	if err != nil {
		t.Fatalf("ListInactiveOwnerVehicles: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("candidates = %d, want 1 — a rider's activity must not defer suspension "+
			"(explicit client ruling; widening the join silently reverses it)", len(got))
	}
}

func TestTelemetrySuspensionRepo_EpisodeLifecycle(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	cleanTables(t, testPool)
	cleanSuspensionTables(t)

	repo := store.NewTelemetrySuspensionRepo(testPool)
	ctx := context.Background()
	now := suspensionNow()

	seedVehicleForOwner(t, testPool, "veh_ep", vinForIndex(660), "user_ep")
	seedActivity(t, "user_ep", now.Add(-6*24*time.Hour))

	warnedAt := now.Add(-25 * time.Hour)
	if err := repo.MarkWarned(ctx, "veh_ep", warnedAt); err != nil {
		t.Fatalf("MarkWarned: %v", err)
	}
	// A re-run must NOT backdate or advance the notice the owner received.
	if err := repo.MarkWarned(ctx, "veh_ep", now); err != nil {
		t.Fatalf("MarkWarned (re-run): %v", err)
	}
	got, err := repo.ListInactiveOwnerVehicles(ctx, now.Add(-4*24*time.Hour), 50)
	if err != nil {
		t.Fatalf("ListInactiveOwnerVehicles: %v", err)
	}
	if len(got) != 1 || got[0].WarnedAt == nil || !got[0].WarnedAt.Equal(warnedAt) {
		t.Fatalf("warned_at after a re-run = %v, want the FIRST instant %v", got, warnedAt)
	}

	// Suspension is likewise write-once: the catalog has already emitted it.
	if err := repo.MarkSuspended(ctx, "veh_ep", now); err != nil {
		t.Fatalf("MarkSuspended: %v", err)
	}
	if err := repo.MarkSuspended(ctx, "veh_ep", now.Add(time.Hour)); err != nil {
		t.Fatalf("MarkSuspended (re-run): %v", err)
	}
	at, ok, err := repo.SuspendedAt(ctx, "veh_ep")
	if err != nil || !ok {
		t.Fatalf("SuspendedAt = (%v, %v, %v)", at, ok, err)
	}
	if !at.Equal(now) {
		t.Errorf("suspended_at = %v, want the FIRST instant %v", at.UTC(), now)
	}

	// The reconnect / teardown clear.
	if err := repo.ClearForVehicle(ctx, "veh_ep"); err != nil {
		t.Fatalf("ClearForVehicle: %v", err)
	}
	if _, ok, _ := repo.SuspendedAt(ctx, "veh_ep"); ok {
		t.Error("suspension survived ClearForVehicle")
	}
	// Idempotent — both callers run against cars that may never have had a row.
	if err := repo.ClearForVehicle(ctx, "veh_ep"); err != nil {
		t.Fatalf("ClearForVehicle (re-run): %v", err)
	}
}

// TestTelemetrySuspensionRepo_ReturnedOwnerResetsOnlyOpenEpisodes is the
// asymmetry the contract turns on: coming back BEFORE suspension ends the
// episode, coming back AFTER it does not.
func TestTelemetrySuspensionRepo_ReturnedOwnerResetsOnlyOpenEpisodes(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping store integration test")
	}
	mustApplyGoMigrations(t)
	cleanTables(t, testPool)
	cleanSuspensionTables(t)

	repo := store.NewTelemetrySuspensionRepo(testPool)
	ctx := context.Background()
	now := suspensionNow()

	seedVehicleForOwner(t, testPool, "veh_open", vinForIndex(670), "user_open")
	seedVehicleForOwner(t, testPool, "veh_closed", vinForIndex(671), "user_closed")
	// Both owners are back today.
	seedActivity(t, "user_open", now)
	seedActivity(t, "user_closed", now)

	if err := repo.MarkWarned(ctx, "veh_open", now.Add(-25*time.Hour)); err != nil {
		t.Fatalf("MarkWarned: %v", err)
	}
	if err := repo.MarkSuspended(ctx, "veh_closed", now.Add(-25*time.Hour)); err != nil {
		t.Fatalf("MarkSuspended: %v", err)
	}

	cleared, err := repo.ClearReturnedOwnerEpisodes(ctx, now.Add(-4*24*time.Hour))
	if err != nil {
		t.Fatalf("ClearReturnedOwnerEpisodes: %v", err)
	}
	if cleared != 1 {
		t.Errorf("cleared = %d, want 1 (only the OPEN episode)", cleared)
	}
	if _, ok, _ := repo.SuspendedAt(ctx, "veh_closed"); !ok {
		t.Error("a SUSPENDED episode was cleared by the owner merely returning — " +
			"the config is gone at Tesla and only an explicit reconnect puts it back, " +
			"so this would erase the disconnect notice while leaving the car silent")
	}
}
