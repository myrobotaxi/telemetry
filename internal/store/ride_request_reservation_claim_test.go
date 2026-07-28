package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// The MYR-179 review round moved two decisions into SQL. These pin both:
//
//	the guarded reservation claim — `status = 'accepted'` re-checked AT CLAIM
//	TIME, so a ride cancelled or advanced in the select→claim window loses;
//	the anti-starvation due filter — a row whose car is busy drops OUT of the
//	oldest-first LIMIT window (so it cannot head-of-line block a younger,
//	dispatchable reservation) but comes BACK the moment it is past its
//	lateness ceiling and must be resolved.

// seedForVehicle is seedReservation with an explicit vehicle, which the
// busy/starvation cases need (minimalRideRequest pins one vehicle id for
// everything).
func seedForVehicle(
	t *testing.T,
	repo *store.RideRequestRepo,
	riderSuffix, vehicleID string,
	s reservationSeed,
) string {
	t.Helper()
	ctx := context.Background()

	rec := minimalRideRequest()
	rec.RiderID = "clrider" + riderSuffix
	rec.VehicleID = vehicleID
	rec.ScheduledFor = s.scheduledFor

	created, err := repo.Create(ctx, rec)
	if err != nil {
		t.Fatalf("Create(%s): %v", riderSuffix, err)
	}
	if _, err := testPool.Exec(ctx,
		`UPDATE go_ride_requests SET status = $2 WHERE id = $1`,
		created.ID, string(s.status),
	); err != nil {
		t.Fatalf("force status %s: %v", s.status, err)
	}
	if s.claimed {
		if _, err := testPool.Exec(ctx,
			`UPDATE go_ride_requests SET dispatched_at = NOW() WHERE id = $1`, created.ID,
		); err != nil {
			t.Fatalf("force claim: %v", err)
		}
	}
	return created.ID
}

func dueIDs(t *testing.T, repo *store.RideRequestRepo, sweepAt time.Time, limit int) []string {
	t.Helper()
	due, err := repo.ListDueReservations(context.Background(), sweepAt, sweepAt.Add(-testExpiryWindow), limit)
	if err != nil {
		t.Fatalf("ListDueReservations: %v", err)
	}
	ids := make([]string, 0, len(due))
	for i := range due {
		ids = append(ids, due[i].ID)
	}
	return ids
}

func contains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// TestListDueReservations_BusyVehicleDropsOutOfTheWindow: a reservation whose
// car is mid-instant-ride is not actionable this pass, so it must not consume a
// slot in the oldest-first LIMIT window. It stays selectable — it simply
// reappears once the car frees up.
func TestListDueReservations_BusyVehicleDropsOutOfTheWindow(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	sweepAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	const vehicle = "clvehiclebusy000000000"

	// The car is on a live INSTANT ride.
	seedForVehicle(t, repo, "0000000000000001", vehicle, reservationSeed{
		store.RideRequestStatusEnroute, nil, false,
	})
	held := seedForVehicle(t, repo, "0000000000000002", vehicle, reservationSeed{
		store.RideRequestStatusAccepted, timePtr(sweepAt.Add(-time.Minute)), false,
	})

	if ids := dueIDs(t, repo, sweepAt, 100); contains(ids, held) {
		t.Errorf("due = %v, want the held reservation %s excluded while its car is busy", ids, held)
	}

	// The instant ride completes: the held reservation becomes actionable again.
	if _, err := testPool.Exec(context.Background(),
		`UPDATE go_ride_requests SET status = 'completed' WHERE scheduled_for IS NULL`,
	); err != nil {
		t.Fatalf("complete the instant ride: %v", err)
	}
	if ids := dueIDs(t, repo, sweepAt, 100); !contains(ids, held) {
		t.Errorf("due = %v, want the reservation %s selected once its car is free", ids, held)
	}
}

// TestListDueReservations_ExpiredRowSurfacesEvenWhenBusy is the other half of
// the anti-starvation clause, and the reason it is an OR rather than a plain
// NOT EXISTS: a reservation past its lateness ceiling must be selected whatever
// the car is doing, or it could never be resolved and would sit silently
// pending forever — the exact failure mode the expiry outcome exists to kill.
func TestListDueReservations_ExpiredRowSurfacesEvenWhenBusy(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	sweepAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	const vehicle = "clvehiclebusy000000000"

	seedForVehicle(t, repo, "0000000000000001", vehicle, reservationSeed{
		store.RideRequestStatusEnroute, nil, false,
	})
	expired := seedForVehicle(t, repo, "0000000000000002", vehicle, reservationSeed{
		// Past sweepAt - testExpiryWindow.
		store.RideRequestStatusAccepted, timePtr(sweepAt.Add(-testExpiryWindow - time.Minute)), false,
	})

	if ids := dueIDs(t, repo, sweepAt, 100); !contains(ids, expired) {
		t.Errorf("due = %v, want the past-ceiling reservation %s selected despite the busy car", ids, expired)
	}
}

// TestListDueReservations_HeldRowsDoNotStarveYoungerDues is the head-of-line
// proof at the LIMIT boundary. Before the review round the oldest-first window
// was filled by rows that could not be acted on, and a younger reservation for
// an IDLE car — a rider standing at the curb — was never even selected.
func TestListDueReservations_HeldRowsDoNotStarveYoungerDues(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	sweepAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	const busyVehicle = "clvehiclebusy000000000"
	const idleVehicle = "clvehicleidle000000000"

	seedForVehicle(t, repo, "0000000000000001", busyVehicle, reservationSeed{
		store.RideRequestStatusEnroute, nil, false,
	})
	// OLDER, but its car is busy — inside the ceiling, so it is held.
	held := seedForVehicle(t, repo, "0000000000000002", busyVehicle, reservationSeed{
		store.RideRequestStatusAccepted, timePtr(sweepAt.Add(-10 * time.Minute)), false,
	})
	// YOUNGER, car idle, rider waiting.
	ready := seedForVehicle(t, repo, "0000000000000003", idleVehicle, reservationSeed{
		store.RideRequestStatusAccepted, timePtr(sweepAt.Add(-1 * time.Minute)), false,
	})

	// A window of ONE: under the old query the held row consumed it.
	ids := dueIDs(t, repo, sweepAt, 1)
	if len(ids) != 1 || ids[0] != ready {
		t.Errorf("due(limit 1) = %v, want only the dispatchable reservation %s "+
			"(the held row %s must not occupy the window)", ids, ready, held)
	}
}

// TestClaimReservationDispatch is the guarded-claim matrix. Every `want:false`
// row is a nav push that must NOT happen.
func TestClaimReservationDispatch(t *testing.T) {
	sweepAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	past := sweepAt.Add(-time.Minute)

	tests := []struct {
		name string
		seed reservationSeed
		want bool
	}{
		{
			name: "an accepted, unclaimed reservation is claimable",
			seed: reservationSeed{store.RideRequestStatusAccepted, timePtr(past), false},
			want: true,
		},
		{
			name: "an already-claimed reservation loses (exactly-once)",
			seed: reservationSeed{store.RideRequestStatusAccepted, timePtr(past), true},
			want: false,
		},
		{
			name: "a reservation cancelled since the sweep listed it loses",
			seed: reservationSeed{store.RideRequestStatusCancelled, timePtr(past), false},
			want: false,
		},
		{
			name: "a reservation the owner already marked picked-up loses",
			seed: reservationSeed{store.RideRequestStatusArrived, timePtr(past), false},
			want: false,
		},
		{
			name: "a reservation whose rider already started loses",
			seed: reservationSeed{store.RideRequestStatusEnroute, timePtr(past), false},
			want: false,
		},
		{
			name: "a declined reservation loses",
			seed: reservationSeed{store.RideRequestStatusDeclined, timePtr(past), false},
			want: false,
		},
		{
			name: "an INSTANT ride is never claimable by the sweeper",
			seed: reservationSeed{store.RideRequestStatusAccepted, nil, false},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, _ := setupRideRequestRepo(t)
			id := seedReservation(t, repo, "0000000000000001", tt.seed)

			got, err := repo.ClaimReservationDispatch(context.Background(), id)
			if err != nil {
				t.Fatalf("ClaimReservationDispatch: %v", err)
			}
			if got != tt.want {
				t.Errorf("claimed = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestClaimReservationDispatch_MissingRowIsNotAnError: a ride deleted between
// the sweep and the claim is an ordinary "not ours", never a failure the
// sweeper should log or retry.
func TestClaimReservationDispatch_MissingRowIsNotAnError(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)

	claimed, err := repo.ClaimReservationDispatch(context.Background(), "clnosuchride0000000000")
	if err != nil {
		t.Fatalf("ClaimReservationDispatch: %v", err)
	}
	if claimed {
		t.Error("claimed = true for a missing row, want false")
	}
}

// TestClaimReservationDispatch_SecondCallLoses proves the latch admits ONE
// winner even when both callers see an accepted reservation — the property that
// lets every replica sweep without coordination.
func TestClaimReservationDispatch_SecondCallLoses(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	sweepAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	id := seedReservation(t, repo, "0000000000000001", reservationSeed{
		store.RideRequestStatusAccepted, timePtr(sweepAt.Add(-time.Minute)), false,
	})

	first, err := repo.ClaimReservationDispatch(context.Background(), id)
	if err != nil || !first {
		t.Fatalf("first claim = %v, %v; want true, nil", first, err)
	}
	second, err := repo.ClaimReservationDispatch(context.Background(), id)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if second {
		t.Error("second claim = true, want false (the latch admits one winner)")
	}
}

// TestClaimReservationDispatch_StampsTheSameLatchTheReconcilerWatches: the
// reservation claim must write the SAME dispatched_at column the instant path
// stamps, or the startup reconciler would never see an interrupted reservation
// dispatch and the row would stay pending forever.
func TestClaimReservationDispatch_StampsTheSameLatchTheReconcilerWatches(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	sweepAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	id := seedReservation(t, repo, "0000000000000001", reservationSeed{
		store.RideRequestStatusAccepted, timePtr(sweepAt.Add(-time.Minute)), false,
	})

	if claimed, err := repo.ClaimReservationDispatch(ctx, id); err != nil || !claimed {
		t.Fatalf("claim = %v, %v; want true, nil", claimed, err)
	}
	// Age the claim past the reconciler's floor and confirm it is visible.
	if _, err := testPool.Exec(ctx,
		`UPDATE go_ride_requests SET dispatched_at = NOW() - INTERVAL '1 hour' WHERE id = $1`, id,
	); err != nil {
		t.Fatalf("age the claim: %v", err)
	}
	ids, err := repo.ListInterruptedDispatches(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("ListInterruptedDispatches: %v", err)
	}
	if !contains(ids, id) {
		t.Errorf("interrupted = %v, want the reservation claim %s to be reconcilable", ids, id)
	}
}
