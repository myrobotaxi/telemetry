package store_test

// MYR-394 — integration tests for the active-ride poll target read path,
// against a real Postgres (testcontainers).
//
// The predicate this exercises decides how much Fleet API budget the server
// spends, so the cases that matter most are the EXCLUSIONS: a reservation
// accepted for next week must not be polled, and neither must a ride that has
// already ended.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

const (
	pollVehicleA = "clpollveh0000000000000a"
	pollVehicleB = "clpollveh0000000000000b"
	pollVINA     = "7SAYGDET7TA613795"
	pollVINB     = "7SAYGDET7TA613796"
)

// seedPollRide creates one ride on a vehicle and drives it to `status`.
// Each ride gets its own rider so the MYR-266 per-rider active-instant guard
// never fires between fixtures.
func seedPollRide(
	t *testing.T,
	repo *store.RideRequestRepo,
	vehicleID, riderID string,
	scheduled bool,
	status store.RideRequestStatus,
) store.RideRequestRecord {
	t.Helper()
	ctx := context.Background()

	rec := store.RideRequestRecord{
		RiderID:   riderID,
		OwnerID:   "clowner1234567890abcdef",
		VehicleID: vehicleID,
		Pickup:    store.RidePlace{Latitude: 37.7749, Longitude: -122.4194, Label: "Pickup"},
		Dropoff:   store.RidePlace{Latitude: 37.7766, Longitude: -122.3946, Label: "Dropoff"},
	}
	if scheduled {
		s := nextFixtureInstant()
		rec.ScheduledFor = &s
	}

	created, err := repo.Create(ctx, rec)
	if err != nil {
		t.Fatalf("Create(%s): %v", status, err)
	}
	if status == store.RideRequestStatusRequested {
		return created
	}
	// UpdateStatus is the UNGUARDED setter — a fixture wants to land on a
	// status, not to re-litigate the transition rules the handlers enforce.
	updated, err := repo.UpdateStatus(ctx, created.ID, status)
	if err != nil {
		t.Fatalf("UpdateStatus(%s): %v", status, err)
	}
	return updated
}

func pollTargetIDs(targets []store.ActiveRidePollTarget) map[string]store.ActiveRidePollTarget {
	byID := make(map[string]store.ActiveRidePollTarget, len(targets))
	for _, tgt := range targets {
		byID[tgt.RideRequestID] = tgt
	}
	return byID
}

// TestListActiveRidePollTargets_IncludesAndExcludes is the predicate's truth
// table. Every row is a decision about whether the server spends a Fleet API
// call every 25 seconds on that car.
func TestListActiveRidePollTargets_IncludesAndExcludes(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	seedVehicle(t, testPool, pollVehicleA, pollVINA)
	t.Cleanup(func() { _, _ = testPool.Exec(ctx, `DELETE FROM "Vehicle"`) })

	tests := []struct {
		name      string
		scheduled bool
		status    store.RideRequestStatus
		// dispatchSent marks a reservation whose pickup nav was delivered —
		// the moment it stops being a booking and becomes a car on the road.
		dispatchSent bool
		want         bool
	}{
		{name: "instant accepted", status: store.RideRequestStatusAccepted, want: true},
		{name: "instant arrived", status: store.RideRequestStatusArrived, want: true},
		{name: "instant enroute", status: store.RideRequestStatusEnroute, want: true},

		{name: "instant requested", status: store.RideRequestStatusRequested},
		{name: "instant completed", status: store.RideRequestStatusCompleted},
		{name: "instant cancelled", status: store.RideRequestStatusCancelled},
		{name: "instant declined", status: store.RideRequestStatusDeclined},

		// The rate-budget case: a booking for next week is NOT a live ride.
		{name: "reservation accepted, not dispatched", scheduled: true, status: store.RideRequestStatusAccepted},
		{
			name: "reservation accepted, nav delivered", scheduled: true,
			status: store.RideRequestStatusAccepted, dispatchSent: true, want: true,
		},
		{name: "reservation enroute", scheduled: true, status: store.RideRequestStatusEnroute, want: true},
		{name: "reservation arrived", scheduled: true, status: store.RideRequestStatusArrived, want: true},
		{name: "reservation completed", scheduled: true, status: store.RideRequestStatusCompleted},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := testPool.Exec(ctx, `DELETE FROM go_ride_requests`); err != nil {
				t.Fatalf("clean: %v", err)
			}
			riderID := fmt.Sprintf("clrider%017d", i)
			rec := seedPollRide(t, repo, pollVehicleA, riderID, tt.scheduled, tt.status)
			if tt.dispatchSent {
				if err := repo.RecordDispatchOutcome(ctx, rec.ID, store.DispatchStatusSent, nil); err != nil {
					t.Fatalf("RecordDispatchOutcome: %v", err)
				}
			}

			targets, err := repo.ListActiveRidePollTargets(ctx, 50)
			if err != nil {
				t.Fatalf("ListActiveRidePollTargets: %v", err)
			}
			_, listed := pollTargetIDs(targets)[rec.ID]
			if listed != tt.want {
				t.Fatalf("listed = %v, want %v (status=%s scheduled=%v dispatch_sent=%v)",
					listed, tt.want, tt.status, tt.scheduled, tt.dispatchSent)
			}
		})
	}
}

// TestListActiveRidePollTargets_ResolvesVIN — the poller is useless without it;
// the Fleet API is keyed by VIN and the ride row only carries a vehicle cuid.
func TestListActiveRidePollTargets_ResolvesVIN(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	seedVehicle(t, testPool, pollVehicleA, pollVINA)
	seedVehicle(t, testPool, pollVehicleB, pollVINB)
	t.Cleanup(func() { _, _ = testPool.Exec(ctx, `DELETE FROM "Vehicle"`) })

	a := seedPollRide(t, repo, pollVehicleA, "clrider1111111111111111", false, store.RideRequestStatusAccepted)
	b := seedPollRide(t, repo, pollVehicleB, "clrider2222222222222222", false, store.RideRequestStatusEnroute)

	targets, err := repo.ListActiveRidePollTargets(ctx, 50)
	if err != nil {
		t.Fatalf("ListActiveRidePollTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(targets))
	}
	byID := pollTargetIDs(targets)
	if got := byID[a.ID]; got.VIN != pollVINA || got.VehicleID != pollVehicleA {
		t.Fatalf("ride A target = %+v, want VIN %s on %s", got, pollVINA, pollVehicleA)
	}
	if got := byID[b.ID]; got.VIN != pollVINB || got.VehicleID != pollVehicleB {
		t.Fatalf("ride B target = %+v, want VIN %s on %s", got, pollVINB, pollVehicleB)
	}
}

// TestListActiveRidePollTargets_SkipsVehicleWithoutVIN — the MYR-257
// provisioning INSERT can seed a placeholder row before Tesla supplies a VIN,
// and there is nothing to read from the Fleet API for one.
func TestListActiveRidePollTargets_SkipsVehicleWithoutVIN(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	seedVehicle(t, testPool, pollVehicleA, "")
	t.Cleanup(func() { _, _ = testPool.Exec(ctx, `DELETE FROM "Vehicle"`) })

	seedPollRide(t, repo, pollVehicleA, "clrider3333333333333333", false, store.RideRequestStatusAccepted)

	targets, err := repo.ListActiveRidePollTargets(ctx, 50)
	if err != nil {
		t.Fatalf("ListActiveRidePollTargets: %v", err)
	}
	if len(targets) != 0 {
		t.Fatalf("targets = %+v, want none for a VIN-less vehicle", targets)
	}
}

// TestListActiveRidePollTargets_Limit — the cap is a blast-radius guard, and a
// non-positive limit must refuse to run an unbounded scan rather than run one.
func TestListActiveRidePollTargets_Limit(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	seedVehicle(t, testPool, pollVehicleA, pollVINA)
	seedVehicle(t, testPool, pollVehicleB, pollVINB)
	t.Cleanup(func() { _, _ = testPool.Exec(ctx, `DELETE FROM "Vehicle"`) })

	seedPollRide(t, repo, pollVehicleA, "clrider4444444444444444", false, store.RideRequestStatusAccepted)
	seedPollRide(t, repo, pollVehicleB, "clrider5555555555555555", false, store.RideRequestStatusAccepted)

	capped, err := repo.ListActiveRidePollTargets(ctx, 1)
	if err != nil {
		t.Fatalf("ListActiveRidePollTargets(1): %v", err)
	}
	if len(capped) != 1 {
		t.Fatalf("targets = %d with limit 1, want 1", len(capped))
	}

	none, err := repo.ListActiveRidePollTargets(ctx, 0)
	if err != nil {
		t.Fatalf("ListActiveRidePollTargets(0): %v", err)
	}
	if len(none) != 0 {
		t.Fatalf("targets = %d with limit 0, want 0 (no unbounded scan)", len(none))
	}
}

// TestListActiveRidePollTargets_TerminalTransitionRemovesTarget is the
// reconcile's reaping input: the moment a ride ends, its car must stop being
// listed, so the next pass cancels the poller.
func TestListActiveRidePollTargets_TerminalTransitionRemovesTarget(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	seedVehicle(t, testPool, pollVehicleA, pollVINA)
	t.Cleanup(func() { _, _ = testPool.Exec(ctx, `DELETE FROM "Vehicle"`) })

	rec := seedPollRide(t, repo, pollVehicleA, "clrider6666666666666666", false, store.RideRequestStatusEnroute)

	targets, err := repo.ListActiveRidePollTargets(ctx, 50)
	if err != nil {
		t.Fatalf("ListActiveRidePollTargets: %v", err)
	}
	if len(targets) != 1 {
		t.Fatalf("targets = %d before completion, want 1", len(targets))
	}

	if _, err := repo.UpdateStatus(ctx, rec.ID, store.RideRequestStatusCompleted); err != nil {
		t.Fatalf("UpdateStatus(completed): %v", err)
	}

	after, err := repo.ListActiveRidePollTargets(ctx, 50)
	if err != nil {
		t.Fatalf("ListActiveRidePollTargets after completion: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("targets = %+v after completion, want none — the poller must be reaped", after)
	}
}

// TestListActiveRidePollTargets_OrdersFreshestFirst — when a pass is truncated
// by the cap it must shed the most NEGLECTED rides and keep the freshest.
//
// This is the opposite of what the query originally did, and the inversion
// mattered. The most recently updated ride is the one that just transitioned —
// the rider has the tracking map open right now. The least recently updated is
// the one most likely to have been abandoned, since a live ride's every step
// writes the row. Ordering oldest-first meant a truncated page was filled with
// precisely the rides whose pollers matter least (MYR-394).
func TestListActiveRidePollTargets_OrdersFreshestFirst(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	seedVehicle(t, testPool, pollVehicleA, pollVINA)
	seedVehicle(t, testPool, pollVehicleB, pollVINB)
	t.Cleanup(func() { _, _ = testPool.Exec(ctx, `DELETE FROM "Vehicle"`) })

	older := seedPollRide(t, repo, pollVehicleA, "clrider7777777777777777", false, store.RideRequestStatusAccepted)
	time.Sleep(10 * time.Millisecond)
	newer := seedPollRide(t, repo, pollVehicleB, "clrider8888888888888888", false, store.RideRequestStatusAccepted)

	targets, err := repo.ListActiveRidePollTargets(ctx, 50)
	if err != nil {
		t.Fatalf("ListActiveRidePollTargets: %v", err)
	}
	if len(targets) != 2 {
		t.Fatalf("targets = %d, want 2", len(targets))
	}
	if targets[0].RideRequestID != newer.ID || targets[1].RideRequestID != older.ID {
		t.Fatalf("order = [%s %s], want freshest-updated first [%s %s]",
			targets[0].RideRequestID, targets[1].RideRequestID, newer.ID, older.ID)
	}

	// And the cap sheds the neglected one, not the fresh one.
	capped, err := repo.ListActiveRidePollTargets(ctx, 1)
	if err != nil {
		t.Fatalf("ListActiveRidePollTargets(1): %v", err)
	}
	if len(capped) != 1 || capped[0].RideRequestID != newer.ID {
		t.Fatalf("truncated page = %+v, want only the freshest ride %s", capped, newer.ID)
	}
}

// TestListActiveRidePollTargets_ExcludesStaleRides is the poll-lifetime
// ceiling, and it is the only thing in this system that ever stops polling an
// abandoned ride.
//
// Every other clause of the predicate is about STATUS, and no status expires on
// its own: there is no automatic terminal transition anywhere in the repo, and
// the reservation sweeper's expire() only reaches rows with dispatched_at IS
// NULL. So a rider no-show leaves the row at `arrived` and a dispatched
// reservation nobody cancels stays `accepted`/`sent` — matching a purely
// status-based predicate for the rest of the database's life, re-adopted after
// every restart, at ~3,500 Fleet API GETs a day each.
func TestListActiveRidePollTargets_ExcludesStaleRides(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()
	seedVehicle(t, testPool, pollVehicleA, pollVINA)
	seedVehicle(t, testPool, pollVehicleB, pollVINB)
	t.Cleanup(func() { _, _ = testPool.Exec(ctx, `DELETE FROM "Vehicle"`) })

	// Two rides that a status-only predicate cannot tell apart.
	abandoned := seedPollRide(t, repo, pollVehicleA, "clrider9999999999999999", false, store.RideRequestStatusArrived)
	live := seedPollRide(t, repo, pollVehicleB, "clridera000000000000000", false, store.RideRequestStatusArrived)

	tests := []struct {
		name string
		age  time.Duration
		want bool
	}{
		{name: "just transitioned", age: time.Minute, want: true},
		{name: "five hours ago, inside the ceiling", age: 5 * time.Hour, want: true},
		{name: "five hours fifty-nine, still inside", age: 5*time.Hour + 59*time.Minute, want: true},
		{name: "six hours one minute, past the ceiling", age: 6*time.Hour + time.Minute},
		{name: "abandoned two days ago", age: 48 * time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// updated_at is the anchor because it is frozen at the row's last
			// REAL transition: a live ride's every step writes the row, while
			// an abandoned one stops being written the moment it is abandoned.
			if _, err := testPool.Exec(ctx,
				`UPDATE go_ride_requests SET updated_at = NOW() - $2::interval WHERE id = $1`,
				abandoned.ID, tt.age.String(),
			); err != nil {
				t.Fatalf("age the ride: %v", err)
			}

			targets, err := repo.ListActiveRidePollTargets(ctx, 50)
			if err != nil {
				t.Fatalf("ListActiveRidePollTargets: %v", err)
			}
			byID := pollTargetIDs(targets)
			if _, listed := byID[abandoned.ID]; listed != tt.want {
				t.Fatalf("ride updated %s ago listed = %v, want %v", tt.age, listed, tt.want)
			}
			// The control never moves: the ceiling must bound neglect, not
			// status. A ride touched a moment ago is always polled.
			if _, listed := byID[live.ID]; !listed {
				t.Fatal("the freshly-updated ride stopped being listed; the ceiling is bounding the wrong thing")
			}
		})
	}
}
