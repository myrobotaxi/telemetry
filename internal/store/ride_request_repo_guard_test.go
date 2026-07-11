package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// Guarded-transition tests (MYR-174/175 check-then-write race fix). The
// guard is a single UPDATE with `WHERE id = $1 AND status = ANY($3)`, so
// concurrent conflicting transitions must serialize in Postgres: exactly one
// wins; every loser gets ErrRideRequestConflict.

func TestRideRequestRepo_UpdateStatusFrom(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	t.Run("legal transition succeeds and stamps accepted_at", func(t *testing.T) {
		// Scheduled so these subtests, which share one repo, don't collide
		// under the one-active-instant-ride guard (MYR-230).
		created, err := repo.Create(ctx, scheduledRideRequest())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := repo.UpdateStatusFrom(ctx, created.ID,
			[]store.RideRequestStatus{store.RideRequestStatusRequested},
			store.RideRequestStatusAccepted)
		if err != nil {
			t.Fatalf("UpdateStatusFrom: %v", err)
		}
		if got.Status != store.RideRequestStatusAccepted {
			t.Errorf("status: %q", got.Status)
		}
		if got.AcceptedAt == nil {
			t.Error("accepted_at not stamped")
		}
	})

	t.Run("illegal from-state returns ErrRideRequestConflict", func(t *testing.T) {
		created, err := repo.Create(ctx, scheduledRideRequest())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		if _, err := repo.UpdateStatusFrom(ctx, created.ID,
			[]store.RideRequestStatus{store.RideRequestStatusRequested},
			store.RideRequestStatusCancelled); err != nil {
			t.Fatalf("first cancel: %v", err)
		}
		_, err = repo.UpdateStatusFrom(ctx, created.ID,
			[]store.RideRequestStatus{store.RideRequestStatusRequested},
			store.RideRequestStatusAccepted)
		if !errors.Is(err, store.ErrRideRequestConflict) {
			t.Fatalf("expected ErrRideRequestConflict, got %v", err)
		}
		if errors.Is(err, sdk.ErrNotFound) {
			t.Error("conflict must not wrap sdk.ErrNotFound")
		}
	})

	t.Run("unknown id returns ErrRideRequestNotFound", func(t *testing.T) {
		_, err := repo.UpdateStatusFrom(ctx, "crr-does-not-exist",
			[]store.RideRequestStatus{store.RideRequestStatusRequested},
			store.RideRequestStatusAccepted)
		if !errors.Is(err, store.ErrRideRequestNotFound) {
			t.Fatalf("expected ErrRideRequestNotFound, got %v", err)
		}
	})
}

// TestRideRequestRepo_UpdateStatusFrom_ConcurrentDoubleAccept is the
// double-tap / two-devices scenario: two accepts race on the same
// `requested` row. Exactly one must win — the dispatch seam (ride.accepted)
// is published per winning transition, so a second winner would mean a
// double Tesla navigation_request push under MYR-176.
func TestRideRequestRepo_UpdateStatusFrom_ConcurrentDoubleAccept(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	created, err := repo.Create(ctx, minimalRideRequest())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	results := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = repo.UpdateStatusFrom(ctx, created.ID,
				[]store.RideRequestStatus{store.RideRequestStatusRequested},
				store.RideRequestStatusAccepted)
		}(i)
	}
	wg.Wait()

	wins, conflicts := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, store.ErrRideRequestConflict):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("expected exactly one winner and one conflict, got wins=%d conflicts=%d", wins, conflicts)
	}

	final, err := repo.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if final.Status != store.RideRequestStatusAccepted || final.AcceptedAt == nil {
		t.Errorf("final row: status=%q acceptedAt=%v", final.Status, final.AcceptedAt)
	}
}

// --- One active instant ride per rider (MYR-230, partial unique index) ---

// TestRideRequestRepo_Create_OneActiveInstant covers the guard's sequential
// behavior: a rider may hold at most one OPEN instant ride, terminal states
// free the slot, and scheduled rides are exempt.
func TestRideRequestRepo_Create_OneActiveInstant(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	t.Run("second open instant is rejected with ErrRideRequestActive", func(t *testing.T) {
		if _, err := repo.Create(ctx, minimalRideRequest()); err != nil {
			t.Fatalf("first Create: %v", err)
		}
		_, err := repo.Create(ctx, minimalRideRequest())
		if !errors.Is(err, store.ErrRideRequestActive) {
			t.Fatalf("expected ErrRideRequestActive, got %v", err)
		}
		if errors.Is(err, sdk.ErrNotFound) {
			t.Error("active-ride conflict must not wrap sdk.ErrNotFound")
		}
	})

	t.Run("GetActiveInstantByRider returns the open instant ride", func(t *testing.T) {
		if _, err := testPool.Exec(ctx, `DELETE FROM go_ride_requests`); err != nil {
			t.Fatalf("clean: %v", err)
		}
		created, err := repo.Create(ctx, minimalRideRequest())
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		got, err := repo.GetActiveInstantByRider(ctx, minimalRideRequest().RiderID)
		if err != nil {
			t.Fatalf("GetActiveInstantByRider: %v", err)
		}
		if got.ID != created.ID {
			t.Errorf("active id: got %q want %q", got.ID, created.ID)
		}
	})

	t.Run("no open instant ride returns ErrRideRequestNotFound", func(t *testing.T) {
		_, err := repo.GetActiveInstantByRider(ctx, "clnobody00000000000000")
		if !errors.Is(err, store.ErrRideRequestNotFound) || !errors.Is(err, sdk.ErrNotFound) {
			t.Fatalf("expected ErrRideRequestNotFound (wrapping sdk.ErrNotFound), got %v", err)
		}
	})

	t.Run("terminal state frees the slot for a new instant ride", func(t *testing.T) {
		if _, err := testPool.Exec(ctx, `DELETE FROM go_ride_requests`); err != nil {
			t.Fatalf("clean: %v", err)
		}
		first, err := repo.Create(ctx, minimalRideRequest())
		if err != nil {
			t.Fatalf("first Create: %v", err)
		}
		// Cancel (terminal) removes it from the guard's partial index.
		if _, err := repo.UpdateStatus(ctx, first.ID, store.RideRequestStatusCancelled); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		if _, err := repo.Create(ctx, minimalRideRequest()); err != nil {
			t.Fatalf("second Create after terminal should succeed: %v", err)
		}
		// And GetActiveInstantByRider now returns the NEW open ride, not the
		// cancelled one.
		got, err := repo.GetActiveInstantByRider(ctx, minimalRideRequest().RiderID)
		if err != nil {
			t.Fatalf("GetActiveInstantByRider: %v", err)
		}
		if got.ID == first.ID {
			t.Error("cancelled ride must not be reported as active")
		}
	})

	t.Run("scheduled rides are exempt and never block", func(t *testing.T) {
		if _, err := testPool.Exec(ctx, `DELETE FROM go_ride_requests`); err != nil {
			t.Fatalf("clean: %v", err)
		}
		sched := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
		mkScheduled := func() store.RideRequestRecord {
			r := minimalRideRequest()
			r.ScheduledFor = &sched
			return r
		}
		// Many scheduled rides for one rider: all allowed.
		for i := 0; i < 3; i++ {
			if _, err := repo.Create(ctx, mkScheduled()); err != nil {
				t.Fatalf("scheduled Create %d: %v", i, err)
			}
		}
		// An open instant ride coexists with the scheduled ones.
		if _, err := repo.Create(ctx, minimalRideRequest()); err != nil {
			t.Fatalf("instant Create alongside scheduled: %v", err)
		}
		// GetActiveInstantByRider returns the INSTANT one, ignoring scheduled.
		got, err := repo.GetActiveInstantByRider(ctx, minimalRideRequest().RiderID)
		if err != nil {
			t.Fatalf("GetActiveInstantByRider: %v", err)
		}
		if got.ScheduledFor != nil {
			t.Errorf("active-instant lookup returned a scheduled ride: %+v", got.ScheduledFor)
		}
	})
}

// TestRideRequestRepo_Create_ConcurrentInstant is the race the guard exists
// for: two instant creates for the same rider fire simultaneously. The
// partial unique index serializes them in Postgres — exactly one INSERT
// wins; the loser's 23505 surfaces as ErrRideRequestActive. This is the
// create-side analogue of the UpdateStatusFrom double-accept race.
func TestRideRequestRepo_Create_ConcurrentInstant(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	results := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = repo.Create(ctx, minimalRideRequest())
		}(i)
	}
	wg.Wait()

	wins, conflicts := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, store.ErrRideRequestActive):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("expected exactly one winner and one conflict, got wins=%d conflicts=%d", wins, conflicts)
	}

	// Exactly one open instant row persisted.
	var n int
	if err := testPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM go_ride_requests WHERE rider_id = $1 AND scheduled_for IS NULL`,
		minimalRideRequest().RiderID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected exactly one persisted instant ride, got %d", n)
	}
}

// TestRideRequestRepo_UpdateStatusFrom_CancelVsDecline races the rider's
// cancel against the owner's decline on the same `requested` row. Exactly
// one transition lands; the loser observes the conflict instead of silently
// overwriting a terminal state.
func TestRideRequestRepo_UpdateStatusFrom_CancelVsDecline(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	created, err := repo.Create(ctx, minimalRideRequest())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var (
		wg         sync.WaitGroup
		cancelErr  error
		declineErr error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, cancelErr = repo.UpdateStatusFrom(ctx, created.ID,
			[]store.RideRequestStatus{store.RideRequestStatusRequested, store.RideRequestStatusAccepted},
			store.RideRequestStatusCancelled)
	}()
	go func() {
		defer wg.Done()
		_, declineErr = repo.UpdateStatusFrom(ctx, created.ID,
			[]store.RideRequestStatus{store.RideRequestStatusRequested},
			store.RideRequestStatusDeclined)
	}()
	wg.Wait()

	winners := 0
	for _, err := range []error{cancelErr, declineErr} {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, store.ErrRideRequestConflict):
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if winners != 1 {
		t.Fatalf("expected exactly one winning transition, got %d (cancel=%v decline=%v)", winners, cancelErr, declineErr)
	}

	final, err := repo.GetByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	wantStatus := store.RideRequestStatusCancelled
	if cancelErr != nil {
		wantStatus = store.RideRequestStatusDeclined
	}
	if final.Status != wantStatus {
		t.Errorf("final status %q does not match the winning transition %q", final.Status, wantStatus)
	}
}
