package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// The LAPSED-dispatch half of the lateness ceiling (MYR-555): reservations that
// WERE dispatched and never became a ride.
//
// The due query's `dispatched_at IS NULL` conjunct is what makes the first
// sweep self-terminating, and it is exactly why these rows had no path to any
// resolution — prod c92b2c86 sat `accepted` hours past scheduled + MaxLateness
// with a rider Live Activity still counting down. These tests pin the
// complementary set and the one-winner resolution that drains it.

// lapsedSeed describes one row the lapsed-selection matrix needs. It extends
// reservationSeed's shape with the two dispatch columns, because the whole
// question here is what the dispatch columns say.
type lapsedSeed struct {
	status         store.RideRequestStatus
	scheduledFor   *time.Time
	dispatched     bool
	dispatchStatus string // "" leaves the column NULL
	dispatchError  string // "" leaves the column NULL
}

// seedLapsed creates a ride through the repo and forces it into the dispatch
// state the case needs. Its own rider per row, for migration 0004's per-rider
// partial unique index — the same reason seedReservation does it.
func seedLapsed(t *testing.T, repo *store.RideRequestRepo, riderSuffix string, s lapsedSeed) string {
	t.Helper()
	ctx := context.Background()

	rec := minimalRideRequest()
	rec.RiderID = "clrider" + riderSuffix
	if s.scheduledFor != nil {
		placeholder := nextFixtureInstant()
		rec.ScheduledFor = &placeholder
	}

	created, err := repo.Create(ctx, rec)
	if err != nil {
		t.Fatalf("Create(%s): %v", riderSuffix, err)
	}
	if s.scheduledFor != nil {
		plantScheduledFor(t, created.ID, s.scheduledFor)
	}
	if _, err := testPool.Exec(ctx,
		`UPDATE go_ride_requests SET status = $2 WHERE id = $1`,
		created.ID, string(s.status),
	); err != nil {
		t.Fatalf("force status %s: %v", s.status, err)
	}
	if s.dispatched {
		if _, err := testPool.Exec(ctx,
			`UPDATE go_ride_requests SET dispatched_at = NOW() WHERE id = $1`, created.ID,
		); err != nil {
			t.Fatalf("force dispatched_at: %v", err)
		}
	}
	if s.dispatchStatus != "" {
		if _, err := testPool.Exec(ctx,
			`UPDATE go_ride_requests SET dispatch_status = $2 WHERE id = $1`,
			created.ID, s.dispatchStatus,
		); err != nil {
			t.Fatalf("force dispatch_status: %v", err)
		}
	}
	if s.dispatchError != "" {
		if _, err := testPool.Exec(ctx,
			`UPDATE go_ride_requests SET dispatch_error = $2 WHERE id = $1`,
			created.ID, s.dispatchError,
		); err != nil {
			t.Fatalf("force dispatch_error: %v", err)
		}
	}
	return created.ID
}

// TestListLapsedDispatches_Selection is the complement matrix. Every row that
// the due query drops for having been claimed must be reachable here, and
// nothing else must be.
func TestListLapsedDispatches_Selection(t *testing.T) {
	sweepAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	ceiling := sweepAt.Add(-testExpiryWindow)
	lapsed := ceiling.Add(-5 * time.Minute) // past the ceiling
	inside := ceiling.Add(5 * time.Minute)  // dispatched, but still in its window
	exactly := ceiling                      // the boundary itself

	tests := []struct {
		name string
		seed lapsedSeed
		want bool
	}{
		{
			name: "dispatched, sent, never driven, past the ceiling",
			seed: lapsedSeed{store.RideRequestStatusAccepted, timePtr(lapsed), true, "sent", ""},
			want: true,
		},
		{
			name: "the ceiling boundary is inclusive",
			seed: lapsedSeed{store.RideRequestStatusAccepted, timePtr(exactly), true, "sent", ""},
			want: true,
		},
		{
			name: "a dispatched reservation still inside its window is left alone",
			seed: lapsedSeed{store.RideRequestStatusAccepted, timePtr(inside), true, "sent", ""},
			want: false,
		},
		{
			name: "a failed nav push on a lapsed reservation also resolves here",
			seed: lapsedSeed{store.RideRequestStatusAccepted, timePtr(lapsed), true, "failed", "vehicle_asleep"},
			want: true,
		},
		{
			name: "a row already carrying the verdict is never re-listed",
			seed: lapsedSeed{store.RideRequestStatusAccepted, timePtr(lapsed), true, "failed", "reservation_expired"},
			want: false,
		},
		{
			name: "an UNDISPATCHED reservation belongs to the first pass, not this one",
			seed: lapsedSeed{store.RideRequestStatusAccepted, timePtr(lapsed), false, "", ""},
			want: false,
		},
		{
			name: "an instant ride has no ceiling to miss",
			seed: lapsedSeed{store.RideRequestStatusAccepted, nil, true, "sent", ""},
			want: false,
		},
		{
			name: "the parties proceeded manually — arrived leaves the set",
			seed: lapsedSeed{store.RideRequestStatusArrived, timePtr(lapsed), true, "sent", ""},
			want: false,
		},
		{
			name: "a ride under way is not a lapsed reservation",
			seed: lapsedSeed{store.RideRequestStatusEnroute, timePtr(lapsed), true, "sent", ""},
			want: false,
		},
		{
			name: "a cancelled reservation needs no verdict",
			seed: lapsedSeed{store.RideRequestStatusCancelled, timePtr(lapsed), true, "sent", ""},
			want: false,
		},
		{
			name: "a completed ride is the opposite of lapsed",
			seed: lapsedSeed{store.RideRequestStatusCompleted, timePtr(lapsed), true, "sent", ""},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, _ := setupRideRequestRepo(t)
			id := seedLapsed(t, repo, "0000000000000001", tt.seed)

			rows, err := repo.ListLapsedDispatches(context.Background(), ceiling, 100)
			if err != nil {
				t.Fatalf("ListLapsedDispatches: %v", err)
			}

			found := false
			for _, r := range rows {
				if r.ID == id {
					found = true
				}
			}
			if found != tt.want {
				t.Errorf("selected = %v, want %v (returned %d rows)", found, tt.want, len(rows))
			}
		})
	}
}

// TestListLapsedDispatches_OrdersOldestFirst: a backlog drains in the order the
// reservations were booked, exactly as the due pass does.
func TestListLapsedDispatches_OrdersOldestFirst(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	sweepAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	ceiling := sweepAt.Add(-testExpiryWindow)

	newest := seedLapsed(t, repo, "0000000000000001", lapsedSeed{
		store.RideRequestStatusAccepted, timePtr(ceiling.Add(-1 * time.Minute)), true, "sent", "",
	})
	oldest := seedLapsed(t, repo, "0000000000000002", lapsedSeed{
		store.RideRequestStatusAccepted, timePtr(ceiling.Add(-6 * time.Hour)), true, "sent", "",
	})

	rows, err := repo.ListLapsedDispatches(context.Background(), ceiling, 100)
	if err != nil {
		t.Fatalf("ListLapsedDispatches: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("lapsed = %d rows, want 2", len(rows))
	}
	if rows[0].ID != oldest || rows[1].ID != newest {
		t.Errorf("order = [%s %s], want oldest first [%s %s]", rows[0].ID, rows[1].ID, oldest, newest)
	}
	if rows[0].VehicleID == "" || rows[0].ScheduledFor.IsZero() {
		t.Errorf("lean projection lost a column: %+v", rows[0])
	}
}

// TestExpireDispatchedReservation_IsOneWinner is the property the statement
// exists for. The usual latch cannot serve — every row here already has
// `dispatched_at` stamped — so the resolving UPDATE carries the guarantee
// itself. Without it the sweeper would re-end the rider's Live Activity every
// 30 seconds for the life of the row.
func TestExpireDispatchedReservation_IsOneWinner(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	ceiling := time.Date(2026, 8, 1, 11, 30, 0, 0, time.UTC)

	id := seedLapsed(t, repo, "0000000000000001", lapsedSeed{
		store.RideRequestStatusAccepted, timePtr(ceiling.Add(-time.Hour)), true, "sent", "",
	})

	first, err := repo.ExpireDispatchedReservation(ctx, id)
	if err != nil {
		t.Fatalf("ExpireDispatchedReservation (first): %v", err)
	}
	if !first {
		t.Fatal("first call did not win the verdict, want true")
	}
	second, err := repo.ExpireDispatchedReservation(ctx, id)
	if err != nil {
		t.Fatalf("ExpireDispatchedReservation (second): %v", err)
	}
	if second {
		t.Error("second call won too, want false — the verdict must be one-winner")
	}

	// The row must now read exactly as a never-dispatched expiry does: three
	// consumers (the §7.21 registration guard, the ETA ticker's exclusion
	// predicate, the retention sweep) key on that exact pair.
	rec, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if rec.DispatchStatus == nil || string(*rec.DispatchStatus) != "failed" {
		t.Errorf("dispatchStatus = %v, want failed", rec.DispatchStatus)
	}
	if rec.DispatchError == nil || *rec.DispatchError != "reservation_expired" {
		t.Errorf("dispatchError = %v, want reservation_expired", rec.DispatchError)
	}
	// The RIDE is untouched. Expiry is a verdict on the dispatch, and the
	// parties may still cancel or proceed manually (rest-api.md §7.8).
	if rec.Status != store.RideRequestStatusAccepted {
		t.Errorf("status = %q, want the ride left at accepted", rec.Status)
	}
	if rec.DispatchedAt == nil {
		t.Error("dispatchedAt cleared, want the original latch preserved")
	}
}

// TestExpireDispatchedReservation_RefusesARescuedRide pins MYR-461's rule at
// the statement level: the moment the parties proceed manually the reservation
// is no longer lapsed, and no verdict may land on it. If it could, the rider's
// Live Activity would be tombstoned in the middle of a real, driven ride.
func TestExpireDispatchedReservation_RefusesARescuedRide(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	ceiling := time.Date(2026, 8, 1, 11, 30, 0, 0, time.UTC)

	tests := []struct {
		name   string
		rider  string
		status store.RideRequestStatus
	}{
		{name: "picked up", rider: "0000000000000001", status: store.RideRequestStatusArrived},
		{name: "under way", rider: "0000000000000002", status: store.RideRequestStatusEnroute},
		{name: "cancelled", rider: "0000000000000003", status: store.RideRequestStatusCancelled},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := seedLapsed(t, repo, tt.rider, lapsedSeed{
				tt.status, timePtr(ceiling.Add(-time.Hour)), true, "sent", "",
			})
			expired, err := repo.ExpireDispatchedReservation(ctx, id)
			if err != nil {
				t.Fatalf("ExpireDispatchedReservation: %v", err)
			}
			if expired {
				t.Errorf("recorded a lapse verdict on a %s ride, want refusal", tt.status)
			}
		})
	}
}

// TestListDueReservations_CarriesTheTripVersion pins MYR-555's other store
// change: the dispatch frame states the ride's CURRENT version, and the frame
// omits it at zero — so a ride edited even once would tell a client holding
// version N that the record had regressed to 0, and the client would refuse the
// very refetch the frame exists to trigger (the MYR-548 defect, on a new path).
func TestListDueReservations_CarriesTheTripVersion(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	sweepAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	id := seedReservation(t, repo, "0000000000000001", reservationSeed{
		store.RideRequestStatusAccepted, timePtr(sweepAt.Add(-5 * time.Minute)), false,
	})
	if _, err := testPool.Exec(ctx,
		`UPDATE go_ride_requests SET trip_version = 5 WHERE id = $1`, id,
	); err != nil {
		t.Fatalf("plant trip_version: %v", err)
	}

	due, err := repo.ListDueReservations(ctx, sweepAt, sweepAt.Add(-testExpiryWindow), 100)
	if err != nil {
		t.Fatalf("ListDueReservations: %v", err)
	}
	if len(due) != 1 {
		t.Fatalf("due = %d rows, want 1", len(due))
	}
	if due[0].TripVersion != 5 {
		t.Errorf("TripVersion = %d, want the row's current 5", due[0].TripVersion)
	}
}
