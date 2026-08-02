package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// MYR-421 — the held completion `end`'s read path against a real database.
//
// A completed ride's `end` push is held for the linger horizon so the sixth
// island expansion's check survives longer than the ~2 seconds an `.ended`
// Activity gets. That means the registration row must SURVIVE those five
// minutes and be findable again afterwards, which is what this query does and
// what these cases pin.

// completeRideAt marks a seeded fixture ride terminal at a given instant, the
// way the guarded status write does.
func completeRideAt(t *testing.T, rideID string, at time.Time) {
	t.Helper()
	if _, err := testPool.Exec(context.Background(),
		`UPDATE go_ride_requests
		    SET status = 'completed', completed_at = $2, updated_at = $2
		  WHERE id = $1`, rideID, at); err != nil {
		t.Fatalf("complete ride %s: %v", rideID, err)
	}
}

// TestLiveActivityRepo_HeldEndWaitsForTheHorizon is the central behaviour: a
// ride that has just completed is NOT due, and the same ride past the horizon
// is. Everything else here is an exclusion.
func TestLiveActivityRepo_HeldEndWaitsForTheHorizon(t *testing.T) {
	const ride = "cride0025h1"
	repo := setupLiveActivities(t, ride)
	ctx := context.Background()

	if err := repo.RegisterActivity(ctx, ride, "rider-1", "token-h1", false); err != nil {
		t.Fatalf("RegisterActivity: %v", err)
	}

	// Just completed — inside the hold.
	completeRideAt(t, ride, time.Now())
	due, err := repo.ListRidesAwaitingActivityEnd(ctx, 5*time.Minute, 10)
	if err != nil {
		t.Fatalf("ListRidesAwaitingActivityEnd: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("a ride that completed a moment ago is already due (%v); the hold IS the time "+
			"the rider gets to see the check on the island", due)
	}

	// Six minutes on — overdue.
	completeRideAt(t, ride, time.Now().Add(-6*time.Minute))
	due, err = repo.ListRidesAwaitingActivityEnd(ctx, 5*time.Minute, 10)
	if err != nil {
		t.Fatalf("ListRidesAwaitingActivityEnd: %v", err)
	}
	if len(due) != 1 || due[0] != ride {
		t.Fatalf("due = %v, want [%s] — a card past its hold must be findable by a fresh process", due, ride)
	}
}

// TestLiveActivityRepo_HeldEndExclusions covers every reason a row is NOT due,
// each of which is load-bearing.
func TestLiveActivityRepo_HeldEndExclusions(t *testing.T) {
	tests := []struct {
		name    string
		ride    string
		arrange func(t *testing.T, repo *store.LiveActivityRepo, ride string)
		wantDue bool
	}{
		{
			// The ordinary due case, here so the table proves the arrangement
			// helpers work rather than only that everything is excluded.
			name: "completed past the horizon is due",
			ride: "cride0025h2",
			arrange: func(t *testing.T, _ *store.LiveActivityRepo, ride string) {
				completeRideAt(t, ride, time.Now().Add(-10*time.Minute))
			},
			wantDue: true,
		},
		{
			// A ride still in flight has an Activity that is still being
			// ticked. Ending it would take the card off the lock screen
			// mid-ride.
			name:    "a ride still in flight is never due",
			ride:    "cride0025h3",
			arrange: func(*testing.T, *store.LiveActivityRepo, string) {},
			wantDue: false,
		},
		{
			// The client's own reaper (MYR-405) or a swipe tombstones the row.
			// A tombstone means the card is already gone; pushing an end at it
			// would be a push nobody is watching for.
			name: "a row the client already ended drops out",
			ride: "cride0025h4",
			arrange: func(t *testing.T, repo *store.LiveActivityRepo, ride string) {
				completeRideAt(t, ride, time.Now().Add(-10*time.Minute))
				if _, err := repo.EndActivity(context.Background(), ride, "rider-1"); err != nil {
					t.Fatalf("EndActivity: %v", err)
				}
			},
			wantDue: false,
		},
		{
			// THE FLOOR, not a fallback anybody expects to use. Every path that
			// sets `completed` also stamps completed_at, but the column is
			// nullable and predates this read — and a completed ride is
			// terminal, so a row invisible here is invisible FOREVER. Falling
			// back to updated_at degrades to "held slightly longer", never to
			// "never ended".
			name: "a completed ride with no completed_at falls back to updated_at",
			ride: "cride0025h5",
			arrange: func(t *testing.T, _ *store.LiveActivityRepo, ride string) {
				if _, err := testPool.Exec(context.Background(),
					`UPDATE go_ride_requests
					    SET status = 'completed', completed_at = NULL, updated_at = $2
					  WHERE id = $1`, ride, time.Now().Add(-10*time.Minute)); err != nil {
					t.Fatalf("complete without a stamp: %v", err)
				}
			},
			wantDue: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := setupLiveActivities(t, tt.ride)
			ctx := context.Background()
			if err := repo.RegisterActivity(ctx, tt.ride, "rider-1", "token-"+tt.ride, false); err != nil {
				t.Fatalf("RegisterActivity: %v", err)
			}
			tt.arrange(t, repo, tt.ride)

			due, err := repo.ListRidesAwaitingActivityEnd(ctx, 5*time.Minute, 10)
			if err != nil {
				t.Fatalf("ListRidesAwaitingActivityEnd: %v", err)
			}
			if gotDue := len(due) == 1 && due[0] == tt.ride; gotDue != tt.wantDue {
				t.Errorf("due = %v, want the ride present = %v", due, tt.wantDue)
			}
		})
	}
}

// TestLiveActivityRepo_HeldEndReportsARideOnce is why the query groups.
//
// The `end` is sent PER RIDE — endRide fans out over every Activity registered
// against it and tombstones them together — so a ride two parties are watching
// must appear once. Twice would be a duplicate fan-out and a duplicate
// tombstone on every such ride.
func TestLiveActivityRepo_HeldEndReportsARideOnce(t *testing.T) {
	const ride = "cride0025h6"
	repo := setupLiveActivities(t, ride)
	ctx := context.Background()

	for _, user := range []string{"rider-1", "owner-1"} {
		if err := repo.RegisterActivity(ctx, ride, user, "token-"+user, false); err != nil {
			t.Fatalf("RegisterActivity(%s): %v", user, err)
		}
	}
	completeRideAt(t, ride, time.Now().Add(-10*time.Minute))

	due, err := repo.ListRidesAwaitingActivityEnd(ctx, 5*time.Minute, 10)
	if err != nil {
		t.Fatalf("ListRidesAwaitingActivityEnd: %v", err)
	}
	if len(due) != 1 {
		t.Errorf("due = %v, want one entry for a two-party ride", due)
	}
}

// TestLiveActivityRepo_HeldEndOrdersOldestFirst pins the ordering the cap
// relies on. It only bites after a restart left a backlog, which is precisely
// when "most overdue first" is the ordering worth having.
func TestLiveActivityRepo_HeldEndOrdersOldestFirst(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping live activity repo test")
	}
	mustApplyGoMigrations(t)
	cleanGoLiveActivities(t)

	repo := newLiveActivityRepo(t)
	ctx := context.Background()

	// Seeded newest-first so a query that preserved insertion order would fail.
	rides := []string{"cride0025h9", "cride0025h8", "cride0025h7"}
	for i, ride := range rides {
		seedActivityRide(t, ride)
		if err := repo.RegisterActivity(ctx, ride, "rider-"+ride, "token-"+ride, false); err != nil {
			t.Fatalf("RegisterActivity(%s): %v", ride, err)
		}
		completeRideAt(t, ride, time.Now().Add(-time.Duration(10+i*10)*time.Minute))
	}

	due, err := repo.ListRidesAwaitingActivityEnd(ctx, 5*time.Minute, 10)
	if err != nil {
		t.Fatalf("ListRidesAwaitingActivityEnd: %v", err)
	}
	want := []string{"cride0025h7", "cride0025h8", "cride0025h9"}
	if len(due) != len(want) {
		t.Fatalf("due = %v, want %v", due, want)
	}
	for i := range want {
		if due[i] != want[i] {
			t.Fatalf("due = %v, want %v (oldest completion first)", due, want)
		}
	}

	// And the cap takes the most overdue, not an arbitrary two.
	capped, err := repo.ListRidesAwaitingActivityEnd(ctx, 5*time.Minute, 2)
	if err != nil {
		t.Fatalf("ListRidesAwaitingActivityEnd(limit=2): %v", err)
	}
	if len(capped) != 2 || capped[0] != want[0] || capped[1] != want[1] {
		t.Errorf("capped = %v, want the two most overdue %v", capped, want[:2])
	}
}

// TestLiveActivityRepo_HeldEndRejectsBadArguments — a non-positive limit is a
// caller bug, and so is a negative hold. Both would otherwise degrade quietly:
// LIMIT 0 returns nothing forever, and a negative interval would end cards
// before they completed.
func TestLiveActivityRepo_HeldEndRejectsBadArguments(t *testing.T) {
	if !dockerAvailable {
		t.Skip("docker unavailable; skipping live activity repo test")
	}
	mustApplyGoMigrations(t)
	repo := newLiveActivityRepo(t)
	ctx := context.Background()

	if _, err := repo.ListRidesAwaitingActivityEnd(ctx, 5*time.Minute, 0); err == nil {
		t.Error("a zero limit was accepted")
	}
	if _, err := repo.ListRidesAwaitingActivityEnd(ctx, -time.Minute, 10); err == nil {
		t.Error("a negative hold was accepted")
	}
}
