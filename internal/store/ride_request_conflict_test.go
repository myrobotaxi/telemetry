package store_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// Integration coverage for the MYR-383 per-vehicle ride-window conflict gate —
// "a vehicle cannot promise two rides in one window" — at BOTH landing sites,
// against a real Postgres.
//
// The rule under test: every OPEN ride on a vehicle occupies a window of
// ±store.RideConflictWindow around its ride instant (`scheduled_for` for a
// reservation, NOW() for an active instant ride), and a reservation whose
// instant falls INSIDE one is refused. Strictly inside — exactly one window
// apart is allowed, which the boundary table below pins from both directions.

// conflictBase is the anchor every case in this file schedules around. Far
// enough from `now` that the ACTIVE-INSTANT arm can never fire by accident;
// tests that WANT that arm build their instants from time.Now() explicitly.
var conflictBase = time.Date(2028, 3, 4, 15, 0, 0, 0, time.UTC)

// bookReservation creates a reservation for the shared fixture vehicle at `at`.
func bookReservation(t *testing.T, repo *store.RideRequestRepo, at time.Time) (store.RideRequestRecord, error) {
	t.Helper()
	rec := minimalRideRequest()
	rec.ScheduledFor = &at
	return repo.Create(context.Background(), rec)
}

// mustBook fails the test if the booking is refused.
func mustBook(t *testing.T, repo *store.RideRequestRepo, at time.Time) store.RideRequestRecord {
	t.Helper()
	rec, err := bookReservation(t, repo, at)
	if err != nil {
		t.Fatalf("Create(%s): %v", at.Format(time.RFC3339), err)
	}
	return rec
}

// mustAccept drives a ride requested->accepted through the BOOKING-LOCKED write
// the accept endpoint uses.
func mustAccept(t *testing.T, repo *store.RideRequestRepo, id string) {
	t.Helper()
	if _, err := repo.UpdateStatusFromUnconflicted(context.Background(), id,
		[]store.RideRequestStatus{store.RideRequestStatusRequested},
		store.RideRequestStatusAccepted); err != nil {
		t.Fatalf("accept %s: %v", id, err)
	}
}

// assertWindowConflict asserts err is the typed refusal and returns it.
func assertWindowConflict(t *testing.T, err error) *store.RideWindowConflictError {
	t.Helper()
	if !errors.Is(err, store.ErrRideWindowConflict) {
		t.Fatalf("expected ErrRideWindowConflict, got %v", err)
	}
	var conflict *store.RideWindowConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected *RideWindowConflictError, got %T: %v", err, err)
	}
	return conflict
}

// TestRideWindowGate_CreateRefusesOverlap is the reported defect, closed: a
// second reservation inside an ACCEPTED reservation's window is refused at
// create instead of surviving until an owner declines it by hand.
func TestRideWindowGate_CreateRefusesOverlap(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)

	held := mustBook(t, repo, conflictBase)
	mustAccept(t, repo, held.ID)

	_, err := bookReservation(t, repo, conflictBase.Add(20*time.Minute))
	conflict := assertWindowConflict(t, err)

	if conflict.ConflictAt == nil || !conflict.ConflictAt.Equal(conflictBase) {
		t.Errorf("ConflictAt = %v, want the held instant %v", conflict.ConflictAt, conflictBase)
	}
	if conflict.Pending {
		t.Error("Pending = true for a conflict with an ACCEPTED reservation")
	}
	assertRideCount(t, 1)
}

// TestRideWindowGate_CreateRefusesPendingClaim: create counts a still-`requested`
// reservation, so a rider is never handed a booking that is going to collide
// with somebody's pending request. The message must not call it "booked" —
// hence the Pending flag.
func TestRideWindowGate_CreateRefusesPendingClaim(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)

	mustBook(t, repo, conflictBase)

	_, err := bookReservation(t, repo, conflictBase.Add(-30*time.Minute))
	conflict := assertWindowConflict(t, err)

	if !conflict.Pending {
		t.Error("Pending = false for a conflict with a still-`requested` reservation")
	}
	assertRideCount(t, 1)
}

// TestRideWindowGate_Boundary walks the window edge from both directions. The
// comparison is STRICTLY inside, so exactly one window apart is a legal
// back-to-back booking and one second closer is not. Expressed in terms of
// store.RideConflictWindow so changing the product guess changes no test.
func TestRideWindowGate_Boundary(t *testing.T) {
	w := store.RideConflictWindow

	tests := []struct {
		name    string
		offset  time.Duration
		refused bool
	}{
		{name: "exactly one window later is allowed", offset: w},
		{name: "exactly one window earlier is allowed", offset: -w},
		{name: "one second inside the window is refused", offset: w - time.Second, refused: true},
		{name: "one second inside the window, earlier, is refused", offset: -w + time.Second, refused: true},
		{name: "one second outside the window is allowed", offset: w + time.Second},
		{name: "the identical instant is refused", offset: 0, refused: true},
		{name: "well clear of the window is allowed", offset: 4 * w},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, _ := setupRideRequestRepo(t)
			held := mustBook(t, repo, conflictBase)
			mustAccept(t, repo, held.ID)

			_, err := bookReservation(t, repo, conflictBase.Add(tt.offset))
			switch {
			case tt.refused && !errors.Is(err, store.ErrRideWindowConflict):
				t.Fatalf("expected refusal at offset %v, got %v", tt.offset, err)
			case !tt.refused && err != nil:
				t.Fatalf("expected the booking to succeed at offset %v, got %v", tt.offset, err)
			}
		})
	}
}

// TestRideWindowGate_ScopeAndRelease: the gate refuses only what it must — the
// same car, in the window, still open. A different car, and a slot freed by a
// terminal transition, book normally. The second half is what makes the refusal
// a DEFERRAL rather than a permanent hold on a slot.
func TestRideWindowGate_ScopeAndRelease(t *testing.T) {
	ctx := context.Background()

	t.Run("another vehicle is unaffected", func(t *testing.T) {
		repo, _ := setupRideRequestRepo(t)
		held := mustBook(t, repo, conflictBase)
		mustAccept(t, repo, held.ID)

		other := minimalRideRequest()
		other.VehicleID = "clothervehicle00000000"
		at := conflictBase.Add(5 * time.Minute)
		other.ScheduledFor = &at
		if _, err := repo.Create(ctx, other); err != nil {
			t.Fatalf("a reservation for a DIFFERENT car must not be refused: %v", err)
		}
	})

	for _, terminal := range []store.RideRequestStatus{
		store.RideRequestStatusDeclined,
		store.RideRequestStatusCancelled,
		store.RideRequestStatusCompleted,
	} {
		t.Run("a "+string(terminal)+" reservation frees its window", func(t *testing.T) {
			repo, _ := setupRideRequestRepo(t)
			held := mustBook(t, repo, conflictBase)
			if _, err := repo.UpdateStatus(ctx, held.ID, terminal); err != nil {
				t.Fatalf("UpdateStatus(%s): %v", terminal, err)
			}
			if _, err := bookReservation(t, repo, conflictBase.Add(10*time.Minute)); err != nil {
				t.Fatalf("the window must be free once the holder is %s: %v", terminal, err)
			}
		})
	}
}

// TestRideWindowGate_ActiveInstantRide is the second arm: a car that is MID
// RIDE cannot also promise a pickup twenty minutes out. The refusal names no
// instant — the conflicting ride is happening now and has none.
func TestRideWindowGate_ActiveInstantRide(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	instant, err := repo.Create(ctx, minimalRideRequest())
	if err != nil {
		t.Fatalf("Create instant: %v", err)
	}

	t.Run("a merely requested instant ride blocks nothing", func(t *testing.T) {
		soon := time.Now().UTC().Add(store.RideConflictWindow / 3)
		if _, err := bookReservation(t, repo, soon); err != nil {
			t.Fatalf("an unaccepted instant ride must not occupy the window: %v", err)
		}
		if _, err := testPool.Exec(ctx, `DELETE FROM go_ride_requests WHERE scheduled_for IS NOT NULL`); err != nil {
			t.Fatalf("clean: %v", err)
		}
	})

	// The car is now COMMITTED to the instant ride.
	mustAccept(t, repo, instant.ID)

	t.Run("a near-term reservation is refused", func(t *testing.T) {
		soon := time.Now().UTC().Add(store.RideConflictWindow / 3)
		conflict := assertWindowConflict(t, mustFail(bookReservation(t, repo, soon)))
		if conflict.ConflictAt != nil {
			t.Errorf("ConflictAt = %v, want nil — an active instant ride has no scheduled instant to name", conflict.ConflictAt)
		}
	})

	t.Run("a reservation beyond the window is allowed", func(t *testing.T) {
		later := time.Now().UTC().Add(2 * store.RideConflictWindow)
		if _, err := bookReservation(t, repo, later); err != nil {
			t.Fatalf("a reservation clear of the active ride must be allowed: %v", err)
		}
	})
}

// TestRideWindowGate_AcceptBackstop covers the second layer. The two
// conflicting reservations are PLANTED (bypassing the create gate) because that
// is exactly what the backstop is for: rows booked before the gate existed.
func TestRideWindowGate_AcceptBackstop(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	first := mustBook(t, repo, conflictBase)
	second := mustBook(t, repo, conflictBase.Add(4*store.RideConflictWindow))
	// Move the second INTO the first's window behind the gate's back.
	collide := conflictBase.Add(10 * time.Minute)
	plantScheduledFor(t, second.ID, &collide)

	// Accepting the first is allowed: a peer that is still `requested` is a
	// claim, not a commitment, and the accept is HOW the slot is decided.
	mustAccept(t, repo, first.ID)

	// The loser is now refused — truthfully, against a COMMITTED holder.
	_, err := repo.UpdateStatusFromUnconflicted(ctx, second.ID,
		[]store.RideRequestStatus{store.RideRequestStatusRequested},
		store.RideRequestStatusAccepted)
	conflict := assertWindowConflict(t, err)
	if conflict.ConflictAt == nil || !conflict.ConflictAt.Equal(conflictBase) {
		t.Errorf("ConflictAt = %v, want %v", conflict.ConflictAt, conflictBase)
	}
	if conflict.Pending {
		t.Error("Pending = true, but the conflicting reservation is accepted")
	}

	// NOTHING moved: a refused accept must leave the row exactly as it was.
	after, err := repo.GetByID(ctx, second.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if after.Status != store.RideRequestStatusRequested {
		t.Errorf("status = %q after a refused accept, want it untouched at `requested`", after.Status)
	}
	if after.AcceptedAt != nil {
		t.Error("accepted_at stamped by a refused accept")
	}
}

// TestRideWindowGate_AcceptLeavesInstantRidesAlone: an INSTANT accept skips the
// window probe entirely, so its behaviour through the booking-locked write is
// byte-identical to UpdateStatusFrom's — including still being refused by the
// per-vehicle one-active-ride index (MYR-266), which remains its guard.
func TestRideWindowGate_AcceptLeavesInstantRidesAlone(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	// A reservation the car is committed to, well inside the window of NOW.
	soon := time.Now().UTC().Add(store.RideConflictWindow / 3)
	reservation := mustBook(t, repo, soon)
	mustAccept(t, repo, reservation.ID)

	instant, err := repo.Create(ctx, minimalRideRequest())
	if err != nil {
		t.Fatalf("Create instant: %v", err)
	}
	if _, err := repo.UpdateStatusFromUnconflicted(ctx, instant.ID,
		[]store.RideRequestStatus{store.RideRequestStatusRequested},
		store.RideRequestStatusAccepted); err != nil {
		t.Fatalf("an INSTANT accept is not window-gated in v1a; got %v", err)
	}
}

// TestRideWindowGate_ConcurrentCreates is the race the advisory lock exists
// for: two conflicting reservations for one car, created at the same instant
// from two connections. A pre-check read would let both through. Exactly one
// must land.
func TestRideWindowGate_ConcurrentCreates(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)

	const racers = 6
	results := make([]error, racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release them together
			rec := minimalRideRequest()
			// Distinct instants, all mutually inside one window — so any two
			// of them conflict and no unique index could express it.
			at := conflictBase.Add(time.Duration(i) * time.Minute)
			rec.ScheduledFor = &at
			_, results[i] = repo.Create(context.Background(), rec)
		}(i)
	}
	close(start)
	wg.Wait()

	wins, refusals := 0, 0
	for i, err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, store.ErrRideWindowConflict):
			refusals++
		default:
			t.Fatalf("racer %d: unexpected error: %v", i, err)
		}
	}
	if wins != 1 || refusals != racers-1 {
		t.Fatalf("expected exactly one winner, got wins=%d refusals=%d", wins, refusals)
	}
	assertRideCount(t, 1)
}

// TestRideWindowGate_ConcurrentAccepts is the same race on the backstop: two
// PLANTED conflicting reservations for one car, accepted simultaneously. The
// accept probe ignores pending peers, so neither sees the other at probe time —
// the per-vehicle lock is the only thing that makes exactly one win.
func TestRideWindowGate_ConcurrentAccepts(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)

	ids := make([]string, 0, 2)
	for i, at := range []time.Time{conflictBase, conflictBase.Add(8 * time.Minute)} {
		rec := mustBook(t, repo, conflictBase.Add(time.Duration(i+1)*4*store.RideConflictWindow))
		planted := at
		plantScheduledFor(t, rec.ID, &planted)
		ids = append(ids, rec.ID)
	}

	results := make([]error, len(ids))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			<-start
			_, results[i] = repo.UpdateStatusFromUnconflicted(context.Background(), id,
				[]store.RideRequestStatus{store.RideRequestStatusRequested},
				store.RideRequestStatusAccepted)
		}(i, id)
	}
	close(start)
	wg.Wait()

	wins, refusals := 0, 0
	for i, err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, store.ErrRideWindowConflict):
			refusals++
		default:
			t.Fatalf("accept %d: unexpected error: %v", i, err)
		}
	}
	if wins != 1 || refusals != 1 {
		t.Fatalf("expected exactly one accepted reservation, got wins=%d refusals=%d", wins, refusals)
	}
}

// TestRideWindowGate_IndexInstalled pins migration 0026: the probe's reservation
// arm must have the partial index that keeps it off a table scan as
// go_ride_requests accumulates terminal rows forever.
func TestRideWindowGate_IndexInstalled(t *testing.T) {
	setupRideRequestRepo(t)

	var def string
	if err := testPool.QueryRow(context.Background(),
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_go_ride_requests_vehicle_window'`,
	).Scan(&def); err != nil {
		t.Fatalf("migration 0026 index missing: %v", err)
	}
	for _, want := range []string{"vehicle_id", "scheduled_for", "requested", "accepted", "arrived", "enroute"} {
		if !strings.Contains(def, want) {
			t.Errorf("index definition is missing %q — the probe would not use it.\n%s", want, def)
		}
	}
}

// mustFail adapts a (record, error) booking result to just its error for the
// assertion helpers. It fails nothing itself — the caller asserts.
func mustFail(_ store.RideRequestRecord, err error) error { return err }

// assertRideCount pins how many rows actually landed, which is the claim a
// race test is really making.
func assertRideCount(t *testing.T, want int) {
	t.Helper()
	var got int
	if err := testPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FROM go_ride_requests`).Scan(&got); err != nil {
		t.Fatalf("count rides: %v", err)
	}
	if got != want {
		t.Fatalf("go_ride_requests holds %d rows, want %d", got, want)
	}
}
