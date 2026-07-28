package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// Reservation-dispatch store paths (MYR-179). These pin the predicates the
// scheduled-dispatch sweeper trusts: "which reservations are actionable now",
// "is this car busy", and the guarded reservation claim. A drift in any of them
// is a dispatch-correctness bug — a reservation dialed early/late, nav
// re-pointed at a car mid-ride, or a cancelled ride dialed anyway.

// testExpiryWindow mirrors the sweeper's production lateness ceiling. The due
// query takes `expiredBefore` (= now - ceiling) rather than the duration, so
// one clock governs selection and the Go-side deadline alike.
const testExpiryWindow = 30 * time.Minute

// reservationSeed describes one row the due-selection matrix needs.
type reservationSeed struct {
	status       store.RideRequestStatus
	scheduledFor *time.Time
	claimed      bool // stamp dispatched_at (the leg-1 latch)
}

// seedReservation creates a ride via the repo (so the coordinates are real
// ciphertext the due query must decrypt) and then forces the row into the
// state the case needs. Every row gets its own rider: migration 0004's
// per-rider partial unique index permits one open INSTANT ride per rider, so
// sharing a rider across the instant cases would 23505 in the fixture.
func seedReservation(t *testing.T, repo *store.RideRequestRepo, riderSuffix string, s reservationSeed) string {
	t.Helper()
	ctx := context.Background()

	rec := minimalRideRequest()
	rec.RiderID = "clrider" + riderSuffix
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

func timePtr(t time.Time) *time.Time { return &t }

// TestListDueReservations_Selection is the due-query matrix. Each case seeds
// exactly one row and asserts whether the sweeper would pick it up.
func TestListDueReservations_Selection(t *testing.T) {
	sweepAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	past := sweepAt.Add(-5 * time.Minute)
	future := sweepAt.Add(5 * time.Minute)

	tests := []struct {
		name string
		seed reservationSeed
		want bool
	}{
		{
			name: "accepted reservation past its scheduled time is due",
			seed: reservationSeed{store.RideRequestStatusAccepted, timePtr(past), false},
			want: true,
		},
		{
			name: "reservation exactly at its scheduled time is due (boundary is inclusive)",
			seed: reservationSeed{store.RideRequestStatusAccepted, timePtr(sweepAt), false},
			want: true,
		},
		{
			name: "reservation still in the future is not due",
			seed: reservationSeed{store.RideRequestStatusAccepted, timePtr(future), false},
			want: false,
		},
		{
			name: "already-claimed reservation is never re-selected",
			seed: reservationSeed{store.RideRequestStatusAccepted, timePtr(past), true},
			want: false,
		},
		{
			name: "instant ride is never swept (it dispatched on accept)",
			seed: reservationSeed{store.RideRequestStatusAccepted, nil, false},
			want: false,
		},
		{
			name: "requested reservation is not due (owner never accepted it)",
			seed: reservationSeed{store.RideRequestStatusRequested, timePtr(past), false},
			want: false,
		},
		{
			name: "cancelled reservation is never dialed",
			seed: reservationSeed{store.RideRequestStatusCancelled, timePtr(past), false},
			want: false,
		},
		{
			name: "declined reservation is never dialed",
			seed: reservationSeed{store.RideRequestStatusDeclined, timePtr(past), false},
			want: false,
		},
		{
			name: "arrived reservation is already past the pickup push",
			seed: reservationSeed{store.RideRequestStatusArrived, timePtr(past), false},
			want: false,
		},
		{
			name: "enroute reservation is already past the pickup push",
			seed: reservationSeed{store.RideRequestStatusEnroute, timePtr(past), false},
			want: false,
		},
		{
			name: "completed reservation is never re-dialed",
			seed: reservationSeed{store.RideRequestStatusCompleted, timePtr(past), false},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, _ := setupRideRequestRepo(t)
			id := seedReservation(t, repo, "0000000000000001", tt.seed)

			due, err := repo.ListDueReservations(context.Background(), sweepAt, sweepAt.Add(-testExpiryWindow), 100)
			if err != nil {
				t.Fatalf("ListDueReservations: %v", err)
			}

			found := false
			for _, d := range due {
				if d.ID == id {
					found = true
				}
			}
			if found != tt.want {
				t.Errorf("selected = %v, want %v (returned %d rows)", found, tt.want, len(due))
			}
		})
	}
}

// TestListDueReservations_OrdersOldestFirstAndDecrypts proves a backlog drains
// in booking order and that the projection carries usable plaintext pickup
// coordinates — the sweeper pushes those to the car, so a decrypt regression
// here would dial the wrong destination.
func TestListDueReservations_OrdersOldestFirstAndDecrypts(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	sweepAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	newest := seedReservation(t, repo, "0000000000000001", reservationSeed{
		store.RideRequestStatusAccepted, timePtr(sweepAt.Add(-1 * time.Minute)), false,
	})
	oldest := seedReservation(t, repo, "0000000000000002", reservationSeed{
		store.RideRequestStatusAccepted, timePtr(sweepAt.Add(-90 * time.Minute)), false,
	})

	due, err := repo.ListDueReservations(context.Background(), sweepAt, sweepAt.Add(-testExpiryWindow), 100)
	if err != nil {
		t.Fatalf("ListDueReservations: %v", err)
	}
	if len(due) != 2 {
		t.Fatalf("due = %d rows, want 2", len(due))
	}
	if due[0].ID != oldest || due[1].ID != newest {
		t.Errorf("order = [%s %s], want oldest reservation first [%s %s]",
			due[0].ID, due[1].ID, oldest, newest)
	}

	want := minimalRideRequest().Pickup
	if due[0].Pickup.Latitude != want.Latitude || due[0].Pickup.Longitude != want.Longitude {
		t.Errorf("pickup = (%v, %v), want the decrypted (%v, %v)",
			due[0].Pickup.Latitude, due[0].Pickup.Longitude, want.Latitude, want.Longitude)
	}
	if due[0].ScheduledFor.IsZero() {
		t.Error("ScheduledFor zero on a due reservation, want the reservation instant")
	}
}

// TestListDueReservations_RespectsLimit proves one sweep cannot become
// unbounded after a backlog builds up.
func TestListDueReservations_RespectsLimit(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	sweepAt := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

	for _, suffix := range []string{"0000000000000001", "0000000000000002", "0000000000000003"} {
		seedReservation(t, repo, suffix, reservationSeed{
			store.RideRequestStatusAccepted, timePtr(sweepAt.Add(-time.Minute)), false,
		})
	}

	due, err := repo.ListDueReservations(context.Background(), sweepAt, sweepAt.Add(-testExpiryWindow), 2)
	if err != nil {
		t.Fatalf("ListDueReservations: %v", err)
	}
	if len(due) != 2 {
		t.Errorf("due = %d rows, want the limit of 2", len(due))
	}
}

// TestVehicleHasActiveInstantRide is the busy-hold predicate matrix. It MUST
// stay character-for-character the MYR-266 per-vehicle index predicate: a
// false negative re-points a car that is carrying someone, a false positive
// strands a reservation until its hold window expires.
func TestVehicleHasActiveInstantRide(t *testing.T) {
	const busyVehicle = "clvehiclebusy000000000"

	tests := []struct {
		name string
		seed reservationSeed
		want bool
	}{
		{"instant accepted ride makes the car busy", reservationSeed{store.RideRequestStatusAccepted, nil, false}, true},
		{"instant arrived ride makes the car busy", reservationSeed{store.RideRequestStatusArrived, nil, false}, true},
		{"instant enroute ride makes the car busy", reservationSeed{store.RideRequestStatusEnroute, nil, false}, true},
		{"a pending request does not make the car busy", reservationSeed{store.RideRequestStatusRequested, nil, false}, false},
		{"a completed ride does not make the car busy", reservationSeed{store.RideRequestStatusCompleted, nil, false}, false},
		{"a cancelled ride does not make the car busy", reservationSeed{store.RideRequestStatusCancelled, nil, false}, false},
		{"a declined ride does not make the car busy", reservationSeed{store.RideRequestStatusDeclined, nil, false}, false},
		{
			name: "an accepted RESERVATION does not make the car busy",
			seed: reservationSeed{store.RideRequestStatusAccepted, timePtr(time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)), false},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, _ := setupRideRequestRepo(t)
			ctx := context.Background()

			rec := minimalRideRequest()
			rec.RiderID = "clrider0000000000000001"
			rec.VehicleID = busyVehicle
			rec.ScheduledFor = tt.seed.scheduledFor
			created, err := repo.Create(ctx, rec)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if _, err := testPool.Exec(ctx,
				`UPDATE go_ride_requests SET status = $2 WHERE id = $1`,
				created.ID, string(tt.seed.status),
			); err != nil {
				t.Fatalf("force status: %v", err)
			}

			busy, err := repo.VehicleHasActiveInstantRide(ctx, busyVehicle)
			if err != nil {
				t.Fatalf("VehicleHasActiveInstantRide: %v", err)
			}
			if busy != tt.want {
				t.Errorf("busy = %v, want %v", busy, tt.want)
			}
		})
	}
}

// TestVehicleHasActiveInstantRide_ScopedToVehicle proves the busy check does
// not leak across cars — one owner's occupied vehicle must not hold up a
// reservation for a different one.
func TestVehicleHasActiveInstantRide_ScopedToVehicle(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	rec := minimalRideRequest()
	rec.VehicleID = "clvehiclebusy000000000"
	created, err := repo.Create(ctx, rec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := testPool.Exec(ctx,
		`UPDATE go_ride_requests SET status = 'enroute' WHERE id = $1`, created.ID,
	); err != nil {
		t.Fatalf("force status: %v", err)
	}

	busy, err := repo.VehicleHasActiveInstantRide(ctx, "clvehicleidle000000000")
	if err != nil {
		t.Fatalf("VehicleHasActiveInstantRide: %v", err)
	}
	if busy {
		t.Error("busy = true for an unrelated vehicle, want false")
	}
}

// TestListInterruptedDispatches_IncludesScheduledRides is the MYR-179
// crash-recovery guarantee, asserted at the source. The sweeper claims the
// SAME leg-1 latch as the instant path, so a crash between its claim and its
// outcome leaves the identical orphan shape (dispatched_at set /
// dispatch_status NULL). This proves the EXISTING leg-1 reconciler query
// already covers scheduled rows — it filters on the latch columns only and
// never mentions scheduled_for — so scheduled dispatch inherits restart
// safety with no widening. If someone ever adds a `scheduled_for IS NULL`
// clause to that query, this test fails and the reservation would silently
// stay stuck-claimed forever.
func TestListInterruptedDispatches_IncludesScheduledRides(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	sched := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	id := seedReservation(t, repo, "0000000000000001", reservationSeed{
		store.RideRequestStatusAccepted, &sched, false,
	})

	// Simulate the sweeper winning the claim and the process dying before it
	// recorded an outcome, aged past the reconciler's floor.
	if _, err := testPool.Exec(ctx,
		`UPDATE go_ride_requests SET dispatched_at = NOW() - INTERVAL '1 hour' WHERE id = $1`, id,
	); err != nil {
		t.Fatalf("simulate interrupted claim: %v", err)
	}

	ids, err := repo.ListInterruptedDispatches(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("ListInterruptedDispatches: %v", err)
	}
	found := false
	for _, got := range ids {
		if got == id {
			found = true
		}
	}
	if !found {
		t.Errorf("interrupted list = %v, want it to include the scheduled ride %s "+
			"(the leg-1 reconciler must cover reservation dispatches)", ids, id)
	}
}
