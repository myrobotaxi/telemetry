package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/store"
)

// Reservation-dormancy tests for the owner pickup transition (MYR-376).
//
// THE DEFECT (verified in production 2026-07-31): an owner accepted a SCHEDULED
// ride due the NEXT DAY and the server allowed accepted→arrived immediately,
// with dispatch_status still NULL because the MYR-179 sweeper had never run.
// The ride stuck at `arrived` with no legal exit.
//
// THE MODEL: a reservation is DORMANT from accept until the EARLIER of its
// dispatch and its due instant, so the pickup transition refuses exactly
// `scheduled_for IS NOT NULL AND dispatch_status IS DISTINCT FROM 'sent'
//  AND scheduled_for > now()`.
//
// The time arm is not a softening — it is the §7.8 reservation-expiry contract:
// a reservation the sweeper failed (`reservation_expired`), skipped, or never
// reached stays `accepted` and its parties "may still cancel or proceed
// manually". Gating on 'sent' alone would strand every such ride with cancel as
// its only exit. The defect is the PRE-DUE pickup and nothing else, which is why
// every scheduled arm below is tested twice — once before due, once after.
//
// The predicate rides INSIDE the guarded UPDATE, which is what these tests
// exercise: not "does a helper return the right bool" but "does the write
// refuse to match a dormant row", including under concurrency.

// dueIn / dueAgo build the explicit due instants these tests turn on. They are
// relative to the wall clock on purpose: the predicate compares `scheduled_for`
// against the DATABASE's NOW(), so a hardcoded calendar date would silently
// change meaning as it aged past the suite's own run date. The offsets are hours
// wide, far beyond any clock skew between the test process and the container.
func dueIn(d time.Duration) *time.Time  { t := time.Now().Add(d); return &t }
func dueAgo(d time.Duration) *time.Time { return dueIn(-d) }

// dormancyRide builds a ride with a caller-chosen rider/vehicle so subtests
// sharing one repo never collide on the per-rider (0004) or per-vehicle (0013)
// one-active-instant-ride indexes — both partial on `scheduled_for IS NULL`, so
// only the INSTANT cases actually need the separation. A nil scheduledFor is an
// INSTANT ride.
func dormancyRide(n int, scheduledFor *time.Time) store.RideRequestRecord {
	rec := minimalRideRequest()
	rec.RiderID = fmt.Sprintf("clrider%019d", n)
	rec.VehicleID = fmt.Sprintf("clvehic%019d", n)
	rec.ScheduledFor = scheduledFor
	return rec
}

// seedAcceptedRide creates a ride, moves it to `accepted` through the ordinary
// guarded transition, and resolves its dispatch outcome the way the MYR-176
// pipeline / MYR-179 sweeper would. A nil dispatch leaves dispatch_status NULL
// — the production defect's exact row shape.
func seedAcceptedRide(t *testing.T, repo *store.RideRequestRepo, n int, scheduledFor *time.Time, dispatch *store.DispatchStatus) string {
	t.Helper()
	ctx := context.Background()
	created, err := repo.Create(ctx, dormancyRide(n, scheduledFor))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := repo.UpdateStatusFrom(ctx, created.ID,
		[]store.RideRequestStatus{store.RideRequestStatusRequested},
		store.RideRequestStatusAccepted); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if dispatch != nil {
		var errCode *string
		if *dispatch == store.DispatchStatusFailed {
			code := "reservation_expired"
			errCode = &code
		}
		if err := repo.RecordDispatchOutcome(ctx, created.ID, *dispatch, errCode); err != nil {
			t.Fatalf("RecordDispatchOutcome(%s): %v", *dispatch, err)
		}
	}
	return created.ID
}

func dispatchStatusPtr(s store.DispatchStatus) *store.DispatchStatus { return &s }

// pickup runs the transition under test: the owner handshake's
// accepted → arrived, through the DORMANCY-guarded write.
func pickup(ctx context.Context, repo *store.RideRequestRepo, id string) (store.RideRequestRecord, error) {
	return repo.UpdateStatusFromDispatched(ctx, id,
		[]store.RideRequestStatus{store.RideRequestStatusAccepted},
		store.RideRequestStatusArrived)
}

// TestRideRequestRepo_UpdateStatusFromDispatched is the dormancy matrix: which
// (due instant, dispatch outcome) rows the pickup write will and will not match.
//
// Every SCHEDULED dispatch outcome appears TWICE — once due in the future, once
// already past due — because that pair IS the contract. Pre-due is the defect;
// post-due is the documented manual-proceed recovery.
func TestRideRequestRepo_UpdateStatusFromDispatched(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	tests := []struct {
		name string
		// scheduledFor is the ride's explicit due instant; nil = INSTANT ride.
		scheduledFor *time.Time
		dispatch     *store.DispatchStatus
		wantErr      error // nil = the pickup must succeed
	}{
		// --- SCHEDULED, still BEFORE its due instant: DORMANT ---
		{
			// The production defect: accepted today, due tomorrow, sweeper
			// never ran. This is the row that used to reach `arrived`.
			name:         "SCHEDULED pre-due with NULL dispatch is dormant",
			scheduledFor: dueIn(24 * time.Hour), dispatch: nil,
			wantErr: store.ErrRideRequestReservationDormant,
		},
		{
			// A failed dispatch BEFORE due is not the expiry case — expiry is
			// only reachable past the due instant — so this row is still
			// dormant, and a pickup a day early is still the defect.
			name:         "SCHEDULED pre-due with failed dispatch is dormant",
			scheduledFor: dueIn(24 * time.Hour), dispatch: dispatchStatusPtr(store.DispatchStatusFailed),
			wantErr: store.ErrRideRequestReservationDormant,
		},
		{
			name:         "SCHEDULED pre-due with skipped dispatch is dormant",
			scheduledFor: dueIn(24 * time.Hour), dispatch: dispatchStatusPtr(store.DispatchStatusSkipped),
			wantErr: store.ErrRideRequestReservationDormant,
		},
		{
			// The sweeper woke it early (accept-after-due, a clock-skewed tick,
			// a reschedule): a dispatched reservation is LIVE whatever the
			// clock says, so `sent` alone carries the pickup.
			name:         "SCHEDULED pre-due with dispatch 'sent' is LIVE and picks up",
			scheduledFor: dueIn(24 * time.Hour), dispatch: dispatchStatusPtr(store.DispatchStatusSent),
			wantErr: nil,
		},

		// --- SCHEDULED, AT/PAST its due instant: the manual-proceed contract ---
		{
			// The sweeper never ran (down, kill-switched, backlogged) and the
			// ride is now owed. §7.8 says the parties may proceed manually, so
			// the owner standing at the car can confirm the pickup.
			name:         "SCHEDULED past-due with NULL dispatch PICKS UP",
			scheduledFor: dueAgo(2 * time.Hour), dispatch: nil,
			wantErr: nil,
		},
		{
			// `reservation_expired` — the lateness ceiling gave up. The DISPATCH
			// failed, the RIDE did not: it stays `accepted`, and refusing the
			// pickup here would leave cancel as its only exit.
			name:         "SCHEDULED past-due with failed dispatch (reservation_expired) PICKS UP",
			scheduledFor: dueAgo(2 * time.Hour), dispatch: dispatchStatusPtr(store.DispatchStatusFailed),
			wantErr: nil,
		},
		{
			name:         "SCHEDULED past-due with skipped dispatch (kill-switch) PICKS UP",
			scheduledFor: dueAgo(2 * time.Hour), dispatch: dispatchStatusPtr(store.DispatchStatusSkipped),
			wantErr: nil,
		},
		{
			// The ordinary happy path: swept at due, nav pushed, owner confirms.
			name:         "SCHEDULED past-due with dispatch 'sent' picks up",
			scheduledFor: dueAgo(2 * time.Hour), dispatch: dispatchStatusPtr(store.DispatchStatusSent),
			wantErr: nil,
		},
		{
			// The boundary itself: `scheduled_for <= NOW()` is inclusive, so a
			// ride due a second ago is already out of dormancy. Kept tight
			// deliberately — a minutes-wide margin here would not distinguish
			// an inclusive comparison from a sloppy one.
			name:         "SCHEDULED just past due picks up (the comparison is inclusive)",
			scheduledFor: dueAgo(time.Second), dispatch: nil,
			wantErr: nil,
		},

		// --- INSTANT: no due instant, no dormancy, ever ---
		{
			// INSTANT rides are entirely unaffected — the car is at the kerb
			// and the owner drives anyway, so a dispatch outcome must never
			// gate their pickup.
			name:         "INSTANT with NULL dispatch picks up",
			scheduledFor: nil, dispatch: nil,
			wantErr: nil,
		},
		{
			name:         "INSTANT with FAILED dispatch picks up",
			scheduledFor: nil, dispatch: dispatchStatusPtr(store.DispatchStatusFailed),
			wantErr: nil,
		},
		{
			name:         "INSTANT with SKIPPED dispatch picks up",
			scheduledFor: nil, dispatch: dispatchStatusPtr(store.DispatchStatusSkipped),
			wantErr: nil,
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id := seedAcceptedRide(t, repo, i, tt.scheduledFor, tt.dispatch)

			got, err := pickup(ctx, repo, id)

			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("pickup: unexpected error %v", err)
				}
				if got.Status != store.RideRequestStatusArrived {
					t.Errorf("status: got %q want %q", got.Status, store.RideRequestStatusArrived)
				}
				assertPickedUpStamped(t, id, true)
				return
			}

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("pickup: got %v, want %v", err, tt.wantErr)
			}
			// A refused pickup must leave NO trace: the row is untouched, so
			// the ride stays exitable (cancel / decline / a later dispatch).
			after, getErr := repo.GetByID(ctx, id)
			if getErr != nil {
				t.Fatalf("GetByID: %v", getErr)
			}
			if after.Status != store.RideRequestStatusAccepted {
				t.Errorf("status must be unchanged: got %q want %q", after.Status, store.RideRequestStatusAccepted)
			}
			assertPickedUpStamped(t, id, false)
		})
	}
}

// assertPickedUpStamped checks the off-wire picked_up_at audit column (P0, not
// on RideRequestRecord) directly — a refused pickup must not stamp it.
func assertPickedUpStamped(t *testing.T, id string, want bool) {
	t.Helper()
	var stamped bool
	if err := testPool.QueryRow(context.Background(),
		`SELECT picked_up_at IS NOT NULL FROM go_ride_requests WHERE id = $1`, id).Scan(&stamped); err != nil {
		t.Fatalf("read picked_up_at: %v", err)
	}
	if stamped != want {
		t.Errorf("picked_up_at stamped=%v, want %v", stamped, want)
	}
}

// TestRideRequestRepo_DormancyLiftsAtDueInstant is the time arm observed on ONE
// row rather than across two seeds: the same undispatched reservation is refused
// before its due instant and accepted after it, with nothing changing in between
// except the clock. This is what makes the refusal a DEFERRAL rather than a dead
// end — the property §7.8's "may still cancel or proceed manually" rests on.
//
// The due instant is seconds out and the retry POLLS rather than sleeping a
// fixed amount, so the test is insensitive to clock skew between this process
// and the database container (the predicate compares against the DB's NOW()).
func TestRideRequestRepo_DormancyLiftsAtDueInstant(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	const window = 3 * time.Second
	id := seedAcceptedRide(t, repo, 500, dueIn(window), nil)

	// Before due, undispatched: the defect's exact shape, refused.
	if _, err := pickup(ctx, repo, id); !errors.Is(err, store.ErrRideRequestReservationDormant) {
		t.Fatalf("pre-due pickup: got %v, want ErrRideRequestReservationDormant", err)
	}
	assertPickedUpStamped(t, id, false)

	// After due, still undispatched: the same call now succeeds. Poll so a slow
	// or skewed container costs time rather than a false failure.
	deadline := time.Now().Add(window + 30*time.Second)
	var got store.RideRequestRecord
	var err error
	for {
		got, err = pickup(ctx, repo, id)
		if err == nil {
			break
		}
		if !errors.Is(err, store.ErrRideRequestReservationDormant) {
			t.Fatalf("post-due pickup: unexpected error %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("dormancy never lifted: pickup still refused %v past due", window)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if got.Status != store.RideRequestStatusArrived {
		t.Errorf("status: got %q want %q", got.Status, store.RideRequestStatusArrived)
	}
	assertPickedUpStamped(t, id, true)
}

// TestRideRequestRepo_UpdateStatusFromDispatched_MissClassification pins that
// the dormancy sentinel never masks the two pre-existing ones: an illegal
// from-status is still a plain conflict (even for a dormant reservation), and
// an unknown id is still not-found. Both map to the same HTTP codes they always
// did — MYR-376 adds no new error code, only a distinguishable reason.
func TestRideRequestRepo_UpdateStatusFromDispatched_MissClassification(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	t.Run("illegal from-status on a dormant reservation is a plain conflict", func(t *testing.T) {
		// Still `requested` AND dormant (due tomorrow, undispatched) — both
		// preconditions fail. Status is the more useful answer, so status wins.
		created, err := repo.Create(ctx, dormancyRide(100, dueIn(24*time.Hour)))
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		_, err = pickup(ctx, repo, created.ID)
		if !errors.Is(err, store.ErrRideRequestConflict) {
			t.Fatalf("got %v, want ErrRideRequestConflict", err)
		}
		if errors.Is(err, store.ErrRideRequestReservationDormant) {
			t.Error("status conflict must not be reported as dormancy")
		}
	})

	t.Run("illegal from-status on a DISPATCHED reservation is a plain conflict", func(t *testing.T) {
		id := seedAcceptedRide(t, repo, 101, dueIn(24*time.Hour), dispatchStatusPtr(store.DispatchStatusSent))
		if _, err := pickup(ctx, repo, id); err != nil {
			t.Fatalf("first pickup: %v", err)
		}
		// Already `arrived` — outside the allowed-from set.
		_, err := pickup(ctx, repo, id)
		if !errors.Is(err, store.ErrRideRequestConflict) {
			t.Fatalf("got %v, want ErrRideRequestConflict", err)
		}
	})

	t.Run("unknown id is not-found", func(t *testing.T) {
		_, err := pickup(ctx, repo, "crr-does-not-exist")
		if !errors.Is(err, store.ErrRideRequestNotFound) {
			t.Fatalf("got %v, want ErrRideRequestNotFound", err)
		}
	})
}

// TestRideRequestRepo_DormantReservationNotStartable is the rider-`start` half
// (MYR-376 requirement 3). Start gets NO new gate and needs none: a dormant
// reservation can never reach `arrived` now that pickup refuses it, so the
// existing from-status guard already makes it unstartable. This asserts the
// property end-to-end at the store: a dormant reservation is `accepted`, the
// start write allows only `arrived`, so it conflicts.
func TestRideRequestRepo_DormantReservationNotStartable(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	id := seedAcceptedRide(t, repo, 200, dueIn(24*time.Hour), nil)

	// The pickup that would have made it startable is refused...
	if _, err := pickup(ctx, repo, id); !errors.Is(err, store.ErrRideRequestReservationDormant) {
		t.Fatalf("pickup: got %v, want ErrRideRequestReservationDormant", err)
	}
	// ...so the rider's start (arrived → enroute) finds nothing to start.
	_, err := repo.UpdateStatusFrom(ctx, id,
		[]store.RideRequestStatus{store.RideRequestStatusArrived},
		store.RideRequestStatusEnroute)
	if !errors.Is(err, store.ErrRideRequestConflict) {
		t.Fatalf("start: got %v, want ErrRideRequestConflict", err)
	}

	final, err := repo.GetByID(ctx, id)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if final.Status != store.RideRequestStatusAccepted {
		t.Errorf("dormant reservation must stay accepted, got %q", final.Status)
	}
}

// TestRideRequestRepo_PickupRacesSweeperDispatch is the concurrency case the
// whole in-the-UPDATE construction exists for: the owner taps "Picked up" at
// the same moment the MYR-179 sweeper dispatches the reservation.
//
// Either interleaving is legitimate — what must NEVER happen is a ride reaching
// `arrived` while dormant. That is the invariant asserted here, over repeated
// rounds: `status = arrived` IMPLIES `dispatch_status = 'sent'`. A check-then-
// write implementation (read dispatch_status, then UPDATE) can violate it; the
// single guarded statement cannot.
func TestRideRequestRepo_PickupRacesSweeperDispatch(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	const rounds = 25
	for i := range rounds {
		// Due tomorrow, so the ONLY thing that can lift dormancy in this round
		// is the sweeper's own write — the clock arm cannot muddy the race.
		id := seedAcceptedRide(t, repo, 300+i, dueIn(24*time.Hour), nil)

		var wg sync.WaitGroup
		var pickupErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, pickupErr = pickup(ctx, repo, id)
		}()
		go func() {
			defer wg.Done()
			// The sweeper waking the reservation.
			if err := repo.RecordDispatchOutcome(ctx, id, store.DispatchStatusSent, nil); err != nil {
				t.Errorf("round %d: sweeper dispatch: %v", i, err)
			}
		}()
		wg.Wait()

		final, err := repo.GetByID(ctx, id)
		if err != nil {
			t.Fatalf("round %d: GetByID: %v", i, err)
		}
		switch final.Status {
		case store.RideRequestStatusArrived:
			// The pickup won the race only because the row was already live.
			if pickupErr != nil {
				t.Fatalf("round %d: row is arrived but pickup returned %v", i, pickupErr)
			}
			if final.DispatchStatus == nil || *final.DispatchStatus != store.DispatchStatusSent {
				t.Fatalf("round %d: INVARIANT VIOLATED — arrived while dormant (dispatch=%v)", i, final.DispatchStatus)
			}
		case store.RideRequestStatusAccepted:
			// The pickup hit the row while it was still dormant.
			if !errors.Is(pickupErr, store.ErrRideRequestReservationDormant) {
				t.Fatalf("round %d: row still accepted but pickup returned %v", i, pickupErr)
			}
		default:
			t.Fatalf("round %d: unexpected final status %q", i, final.Status)
		}
	}
}

// TestRideRequestRepo_ConcurrentDoublePickup is the double-tap / two-devices
// case on a LIVE reservation: the dormancy predicate must not weaken the
// existing exactly-one-winner guarantee of the guarded write.
func TestRideRequestRepo_ConcurrentDoublePickup(t *testing.T) {
	repo, _ := setupRideRequestRepo(t)
	ctx := context.Background()

	// Due tomorrow but already dispatched — LIVE on the `sent` arm alone, so the
	// double-tap is arbitrated purely by the status guard.
	id := seedAcceptedRide(t, repo, 400, dueIn(24*time.Hour), dispatchStatusPtr(store.DispatchStatusSent))

	results := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = pickup(ctx, repo, id)
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
}
