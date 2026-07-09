package store_test

import (
	"context"
	"errors"
	"sync"
	"testing"

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
		created, err := repo.Create(ctx, minimalRideRequest())
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
		created, err := repo.Create(ctx, minimalRideRequest())
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
