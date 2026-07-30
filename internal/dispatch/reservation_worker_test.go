package dispatch

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// The MYR-179 review round hardened four things the original sweeper got
// wrong. These tests pin each one to a behaviour rather than to an
// implementation detail:
//
//	F1  the claim is JUST-IN-TIME — inside the worker, after the busy check,
//	    immediately before the push, so claim→outcome is bounded and a deploy
//	    can never strand a batch of latched-but-unpushed rows.
//	F2  status is re-validated AT CLAIM TIME, so a ride cancelled (or advanced)
//	    in the select→claim window loses.
//	F3  the lateness ceiling applies on the FREE-vehicle path too.
//	F4  `ride.due` fires only when the push actually resolved `sent`.
//	F6  the sweeper's worker budget is its OWN — a saturated sweep never
//	    delays an instant dispatch.

// --- F1: just-in-time claiming ---------------------------------------------

// TestSweep_ClaimsJustInTimeAfterTheBusyCheck pins the ordering that makes the
// 5-minute reconciler floor sound again: for each reservation the worker
// probes busy, THEN claims, THEN records — all inside one worker. The original
// sweeper claimed every row up front in the pass and queued the pushes, which
// left the claim→outcome window unbounded.
func TestSweep_ClaimsJustInTimeAfterTheBusyCheck(t *testing.T) {
	r := testReservation()
	latch := newLatchStore()
	resStore := &fakeReservationStore{due: []DueReservation{r}, busy: map[string]bool{}}
	s, _ := newSweeperHarness(t, latch, resStore, &fakeBus{}, func() time.Time { return testSweepNow }, true)

	s.sweepOnce(context.Background())

	want := []string{"busy:" + r.VehicleID, "claim:" + r.RideRequestID, "record:" + r.RideRequestID}
	got := latch.order()
	if len(got) != len(want) {
		t.Fatalf("call order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call order = %v, want %v", got, want)
		}
	}
}

// TestSweep_HeldRowsAreNeverClaimed is the same invariant stated negatively and
// at batch scale: a pass over many due reservations whose cars are all busy
// must claim NOTHING. Under the pre-review design every one of these rows would
// have been latched and then queued behind a 4-slot pool, so a deploy mid-drain
// burned the whole batch.
func TestSweep_HeldRowsAreNeverClaimed(t *testing.T) {
	latch := newLatchStore()
	due := make([]DueReservation, 0, 10)
	busy := map[string]bool{}
	for i := range 10 {
		r := testReservation()
		r.RideRequestID = "cride" + string(rune('a'+i))
		r.VehicleID = "cveh" + string(rune('a'+i))
		busy[r.VehicleID] = true
		due = append(due, r)
	}
	resStore := &fakeReservationStore{due: due, busy: busy}
	s, exec := newSweeperHarness(t, latch, resStore, &fakeBus{}, func() time.Time { return testSweepNow }, true)

	res := s.sweepOnce(context.Background())

	if res.held != 10 || res.dispatched != 0 || res.expired != 0 {
		t.Errorf("sweep = %+v, want all ten held", res)
	}
	if claims, _, recorded := latch.snapshot(); claims != 0 || len(recorded) != 0 {
		t.Errorf("claims = %d, recorded = %v, want every row untouched", claims, recorded)
	}
	if got := len(exec.calls()); got != 0 {
		t.Errorf("Tesla calls = %d, want 0", got)
	}
}

// TestSweep_ShutdownStopsClaimingNewReservations proves the pass gives
// candidates BACK on shutdown instead of latching them on the way out. A
// cancelled sweep context is the deploy case: the row stays unclaimed and the
// next process re-selects it, rather than becoming an orphan too young for the
// reconciler's 5-minute floor.
func TestSweep_ShutdownStopsClaimingNewReservations(t *testing.T) {
	latch := newLatchStore()
	due := make([]DueReservation, 0, 5)
	for i := range 5 {
		r := testReservation()
		r.RideRequestID = "cride" + string(rune('a'+i))
		due = append(due, r)
	}
	resStore := &fakeReservationStore{due: due, busy: map[string]bool{}}
	s, exec := newSweeperHarness(t, latch, resStore, &fakeBus{}, func() time.Time { return testSweepNow }, true)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // SIGTERM arrived before the workers got their slots

	res := s.sweepOnce(ctx)

	if res.dispatched != 0 || res.held != 5 {
		t.Errorf("sweep = %+v, want every candidate handed back, none claimed", res)
	}
	if claims, _, _ := latch.snapshot(); claims != 0 {
		t.Errorf("claim attempts after cancellation = %d, want 0", claims)
	}
	if got := len(exec.calls()); got != 0 {
		t.Errorf("Tesla calls = %d, want 0", got)
	}
}

// TestSweep_WorkContextSurvivesSweepTimeout proves the per-reservation work is
// NOT bounded by the pass budget. SweepTimeout bounds the due-list query only;
// a push that outlives it must still run to completion and record an outcome,
// or the row would be left claimed-but-unresolved by design.
func TestSweep_WorkContextSurvivesSweepTimeout(t *testing.T) {
	latch := newLatchStore()
	resStore := &fakeReservationStore{due: []DueReservation{testReservation()}, busy: map[string]bool{}}
	s, exec := newSweeperHarness(t, latch, resStore, &fakeBus{}, func() time.Time { return testSweepNow }, true)
	s.cfg.SweepTimeout = time.Nanosecond // already expired by the time the worker runs

	res := s.sweepOnce(context.Background())

	if res.dispatched != 1 {
		t.Fatalf("sweep = %+v, want the reservation dispatched despite the tiny sweep budget", res)
	}
	if got := len(exec.calls()); got != 1 {
		t.Errorf("Tesla calls = %d, want 1", got)
	}
	if _, _, recorded := latch.snapshot(); len(recorded) != 1 || recorded[0].status != OutcomeSent {
		t.Errorf("recorded = %v, want one sent outcome", recorded)
	}
}

// --- F2: status re-validation at claim time --------------------------------

// TestSweep_RideMovedOnBetweenSelectAndClaimLoses is the cancelled-mid-sweep
// case. The rider cancels (or the owner marks picked-up) after the pass
// SELECTed the row; the claim's `status = 'accepted'` conjunct matches no row,
// so the sweeper does nothing at all — no push, no outcome, no ride.due. The
// pre-review claim guarded only `dispatched_at IS NULL` and dialed the car.
func TestSweep_RideMovedOnBetweenSelectAndClaimLoses(t *testing.T) {
	tests := []struct {
		name string
		when string // "before the busy check" | "after the busy check"
	}{
		{"rider cancels before the busy probe", "before the busy check"},
		{"rider cancels between the busy probe and the claim", "after the busy check"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := testReservation()
			latch := newLatchStore()
			resStore := &fakeReservationStore{due: []DueReservation{r}, busy: map[string]bool{}}
			bus := &fakeBus{}

			switch tt.when {
			case "before the busy check":
				resStore.beforeBusy = func(string) { latch.moveOn(r.RideRequestID) }
			case "after the busy check":
				latch.beforeClaim = func(rideID string) { latch.moveOn(rideID) }
			}

			s, exec := newSweeperHarness(t, latch, resStore, bus, func() time.Time { return testSweepNow }, true)
			res := s.sweepOnce(context.Background())

			if res.lost != 1 || res.dispatched != 0 {
				t.Errorf("sweep = %+v, want the claim lost", res)
			}
			if _, won, recorded := latch.snapshot(); won != 0 || len(recorded) != 0 {
				t.Errorf("won = %d, recorded = %v, want the moved-on row untouched", won, recorded)
			}
			if got := len(exec.calls()); got != 0 {
				t.Errorf("Tesla calls = %d, want 0 (the car must not be dialed for a "+
					"cancelled or already-boarded ride)", got)
			}
			if got := len(bus.dueEvents()); got != 0 {
				t.Errorf("ride.due events = %d, want 0", got)
			}
		})
	}
}

// TestSweep_ExpiryOfAMovedOnRideRecordsNothing: the expiry path claims too, so
// it carries the same guard. A reservation the rider already cancelled must not
// be stamped with a dispatch failure it never had.
func TestSweep_ExpiryOfAMovedOnRideRecordsNothing(t *testing.T) {
	r := testReservation()
	latch := newLatchStore()
	latch.moveOn(r.RideRequestID)
	resStore := &fakeReservationStore{due: []DueReservation{r}, busy: map[string]bool{r.VehicleID: true}}
	now := r.ScheduledFor.Add(testMaxLateness + time.Minute)
	s, _ := newSweeperHarness(t, latch, resStore, &fakeBus{}, func() time.Time { return now }, true)

	res := s.sweepOnce(context.Background())

	if res.lost != 1 || res.expired != 0 {
		t.Errorf("sweep = %+v, want the expiry claim lost", res)
	}
	if _, _, recorded := latch.snapshot(); len(recorded) != 0 {
		t.Errorf("recorded = %v, want none on a cancelled ride", recorded)
	}
}

// --- F3: the lateness ceiling on the free path -----------------------------

// TestSweep_LatenessCeilingAppliesToFreeVehicles is the post-downtime /
// post-kill-switch case: the car is IDLE and the reservation is hours stale.
// The pre-review sweeper evaluated expiry only inside the `if busy` branch and
// happily dialed a 14-hour-old pickup. The ceiling now applies regardless of
// vehicle state, and the boundary is exact.
func TestSweep_LatenessCeilingAppliesToFreeVehicles(t *testing.T) {
	r := testReservation()

	tests := []struct {
		name          string
		now           time.Time
		wantDispatch  bool
		wantOutcome   Outcome
		wantErrorCode string
	}{
		{
			name:         "just inside the ceiling still dispatches",
			now:          r.ScheduledFor.Add(testMaxLateness - time.Second),
			wantDispatch: true,
			wantOutcome:  OutcomeSent,
		},
		{
			name:         "exactly at the ceiling still dispatches (the boundary is inclusive)",
			now:          r.ScheduledFor.Add(testMaxLateness),
			wantDispatch: true,
			wantOutcome:  OutcomeSent,
		},
		{
			name:          "one second past the ceiling expires instead",
			now:           r.ScheduledFor.Add(testMaxLateness + time.Second),
			wantOutcome:   OutcomeFailed,
			wantErrorCode: codeReservationExpired,
		},
		{
			name:          "hours past the ceiling after downtime expires, it does not dial a stale pickup",
			now:           r.ScheduledFor.Add(14 * time.Hour),
			wantOutcome:   OutcomeFailed,
			wantErrorCode: codeReservationExpired,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			latch := newLatchStore()
			// The vehicle is FREE in every case — that is the point.
			resStore := &fakeReservationStore{due: []DueReservation{r}, busy: map[string]bool{}}
			bus := &fakeBus{}
			s, exec := newSweeperHarness(t, latch, resStore, bus, func() time.Time { return tt.now }, true)

			res := s.sweepOnce(context.Background())

			wantCalls, wantDue, wantDispatched, wantExpired := 0, 0, 0, 1
			if tt.wantDispatch {
				wantCalls, wantDue, wantDispatched, wantExpired = 1, 1, 1, 0
			}
			if res.dispatched != wantDispatched || res.expired != wantExpired {
				t.Errorf("sweep = %+v, want dispatched=%d expired=%d", res, wantDispatched, wantExpired)
			}
			if got := len(exec.calls()); got != wantCalls {
				t.Errorf("Tesla calls = %d, want %d", got, wantCalls)
			}
			if got := len(bus.dueEvents()); got != wantDue {
				t.Errorf("ride.due events = %d, want %d", got, wantDue)
			}
			_, _, recorded := latch.snapshot()
			if len(recorded) != 1 {
				t.Fatalf("recorded = %v, want exactly one resolved outcome", recorded)
			}
			if recorded[0].status != tt.wantOutcome || recorded[0].code != tt.wantErrorCode {
				t.Errorf("recorded = %+v, want %s/%q", recorded[0], tt.wantOutcome, tt.wantErrorCode)
			}
		})
	}
}

// TestSweep_LatenessIsJudgedInTheWorkerNotAtPassStart: a candidate that waited
// behind the sweeper's worker budget is judged on when it is about to be
// pushed, not on when the pass started. Otherwise a queued row could dispatch
// minutes after crossing its own ceiling.
func TestSweep_LatenessIsJudgedInTheWorkerNotAtPassStart(t *testing.T) {
	r := testReservation()
	latch := newLatchStore()
	resStore := &fakeReservationStore{due: []DueReservation{r}, busy: map[string]bool{}}

	// Pass start: comfortably inside the window. By the time the worker reads
	// the clock again it has crossed the ceiling.
	var (
		mu    sync.Mutex
		calls int
	)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 1 {
			return r.ScheduledFor.Add(time.Minute)
		}
		return r.ScheduledFor.Add(testMaxLateness + time.Minute)
	}
	s, exec := newSweeperHarness(t, latch, resStore, &fakeBus{}, clock, true)

	res := s.sweepOnce(context.Background())

	if res.expired != 1 || res.dispatched != 0 {
		t.Errorf("sweep = %+v, want the worker to expire it on the re-read clock", res)
	}
	if got := len(exec.calls()); got != 0 {
		t.Errorf("Tesla calls = %d, want 0", got)
	}
}

// --- F4: ride.due only on a delivered push ---------------------------------

// TestSweep_RideDuePublishesOnlyOnSent pins the topic contract. `ride.due` is
// the "your car is on the way" seam, and the latch admits one winner — so a
// false event can never be corrected by a later one. It must therefore follow
// the push, not the claim: a kill-switched (`skipped`), token-failed or
// command-failed reservation emits nothing.
func TestSweep_RideDuePublishesOnlyOnSent(t *testing.T) {
	tests := []struct {
		name            string
		dispatchEnabled bool
		execErr         error
		wantOutcome     Outcome
		wantDueEvents   int
	}{
		{
			name:            "delivered push publishes exactly one ride.due",
			dispatchEnabled: true,
			wantOutcome:     OutcomeSent,
			wantDueEvents:   1,
		},
		{
			name:            "DISPATCH_ENABLED=false records skipped and publishes nothing",
			dispatchEnabled: false,
			wantOutcome:     OutcomeSkipped,
			wantDueEvents:   0,
		},
		{
			name:            "a failed command publishes nothing — no car is coming",
			dispatchEnabled: true,
			execErr:         cmdErr(wserrors.ErrCodeKeyNotPaired, false),
			wantOutcome:     OutcomeFailed,
			wantDueEvents:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			latch := newLatchStore()
			resStore := &fakeReservationStore{due: []DueReservation{testReservation()}, busy: map[string]bool{}}
			bus := &fakeBus{}
			s, exec := newSweeperHarness(t, latch, resStore, bus, func() time.Time { return testSweepNow }, tt.dispatchEnabled)
			exec.err = tt.execErr

			s.sweepOnce(context.Background())

			_, _, recorded := latch.snapshot()
			if len(recorded) != 1 || recorded[0].status != tt.wantOutcome {
				t.Fatalf("recorded = %v, want one %s outcome", recorded, tt.wantOutcome)
			}
			if got := len(bus.dueEvents()); got != tt.wantDueEvents {
				t.Errorf("ride.due events = %d, want %d", got, tt.wantDueEvents)
			}
		})
	}
}

// TestSweep_TokenFailurePublishesNoRideDue is the same contract on the
// resolution half of the pipeline: an owner whose Tesla link expired must not
// have their rider told a car is on the way.
func TestSweep_TokenFailurePublishesNoRideDue(t *testing.T) {
	latch := newLatchStore()
	resStore := &fakeReservationStore{due: []DueReservation{testReservation()}, busy: map[string]bool{}}
	bus := &fakeBus{}
	exec := &syncExecutor{}
	d := New(
		&fakeVehicleResolver{vin: "5YJ3E1EA7KF000001"},
		&fakeTokenSource{err: ErrTokenExpired},
		exec,
		latch,
		Config{Enabled: true, MaxRetries: 0, Backoff: time.Millisecond},
		nil,
	)
	resStore.latch = latch
	s := NewReservationSweeper(d, resStore, bus, ReservationConfig{
		Enabled: true, Interval: time.Millisecond, MaxLateness: testMaxLateness, SweepTimeout: 5 * time.Second,
	}, nil).withClock(func() time.Time { return testSweepNow })

	s.sweepOnce(context.Background())

	_, _, recorded := latch.snapshot()
	if len(recorded) != 1 || recorded[0].status != OutcomeFailed {
		t.Fatalf("recorded = %v, want one failed outcome", recorded)
	}
	if got := len(bus.dueEvents()); got != 0 {
		t.Errorf("ride.due events = %d, want 0 when the push never reached the car", got)
	}
	if got := len(exec.calls()); got != 0 {
		t.Errorf("Tesla calls = %d, want 0", got)
	}
}

// --- F6: pool isolation ----------------------------------------------------

// TestSweep_SaturatedSweeperDoesNotDelayInstantDispatch is the isolation
// guarantee. A backlog of reservations whose vehicles are asleep saturates the
// sweeper's own small budget; an INSTANT accept arriving meanwhile must
// dispatch immediately, because it draws on the dispatcher's pool, which the
// sweeper never touches. Before the split both shared four slots and an
// instant rider waited behind the whole reservation queue.
func TestSweep_SaturatedSweeperDoesNotDelayInstantDispatch(t *testing.T) {
	latch := newLatchStore()
	due := make([]DueReservation, 0, 8)
	for i := range 8 {
		r := testReservation()
		r.RideRequestID = "cresv" + string(rune('a'+i))
		r.VehicleID = "cvehresv" + string(rune('a'+i))
		due = append(due, r)
	}
	resStore := &fakeReservationStore{due: due, busy: map[string]bool{}}
	s, exec := newSweeperHarness(t, latch, resStore, &fakeBus{}, func() time.Time { return testSweepNow }, true)

	// Every reservation push blocks until the test releases it.
	release := make(chan struct{})
	exec.mu.Lock()
	exec.block = release
	exec.mu.Unlock()

	sweepDone := make(chan struct{})
	go func() {
		s.sweepOnce(context.Background())
		close(sweepDone)
	}()

	// Wait until the sweeper's budget is fully occupied.
	deadline := time.After(3 * time.Second)
	for len(exec.calls()) < s.cfg.MaxConcurrent {
		select {
		case <-deadline:
			t.Fatal("the sweeper never saturated its worker budget")
		case <-time.After(time.Millisecond):
		}
	}

	// An instant accept lands now. It must not queue behind the sweep.
	instantLatch := newLatchStore()
	instantExec := &syncExecutor{}
	instant := New(
		&fakeVehicleResolver{vin: "5YJ3E1EA7KF000002"},
		&fakeTokenSource{token: "tok"},
		instantExec,
		instantLatch,
		Config{Enabled: true, MaxRetries: 0, Backoff: time.Millisecond},
		nil,
	)
	instantDone := make(chan struct{})
	go func() {
		instant.handle(events.NewEvent(testEvent()))
		instant.Wait()
		close(instantDone)
	}()
	select {
	case <-instantDone:
	case <-time.After(3 * time.Second):
		t.Fatal("an instant dispatch was blocked behind the saturated reservation sweep")
	}
	if _, won, recorded := instantLatch.snapshot(); won != 1 || len(recorded) != 1 || recorded[0].status != OutcomeSent {
		t.Errorf("instant dispatch: won = %d, recorded = %v, want one sent outcome", won, recorded)
	}

	// The sweeper never ran more than its own budget at once.
	if got := len(exec.calls()); got > s.cfg.MaxConcurrent {
		t.Errorf("concurrent reservation pushes = %d, want at most MaxConcurrent = %d", got, s.cfg.MaxConcurrent)
	}

	close(release)
	select {
	case <-sweepDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the sweep never drained after the pushes were released")
	}
}

// TestSweep_ClaimErrorHoldsRatherThanBurning: a claim that ERRORS (as opposed
// to matching no row) leaves the reservation recoverable — it is held, not
// counted as dispatched, and the next tick retries.
func TestSweep_ClaimErrorHoldsRatherThanBurning(t *testing.T) {
	latch := newLatchStore()
	latch.claimErr = errors.New("deadlock detected")
	resStore := &fakeReservationStore{due: []DueReservation{testReservation()}, busy: map[string]bool{}}
	s, exec := newSweeperHarness(t, latch, resStore, &fakeBus{}, func() time.Time { return testSweepNow }, true)

	res := s.sweepOnce(context.Background())

	if res.held != 1 || res.dispatched != 0 {
		t.Errorf("sweep = %+v, want held on a claim error", res)
	}
	if _, _, recorded := latch.snapshot(); len(recorded) != 0 {
		t.Errorf("recorded = %v, want none", recorded)
	}
	if got := len(exec.calls()); got != 0 {
		t.Errorf("Tesla calls = %d, want 0", got)
	}
}

// TestReservationExpiredCodeIsOpaque guards the data-classification rule that
// dispatch_error carries an opaque CODE — never a coordinate, address, token
// or VIN (Rule CG-DC-2).
func TestReservationExpiredCodeIsOpaque(t *testing.T) {
	if strings.ContainsAny(codeReservationExpired, " ,.:/@") {
		t.Errorf("codeReservationExpired = %q, want a bare snake_case code", codeReservationExpired)
	}
	if codeReservationExpired != "reservation_expired" {
		t.Errorf("codeReservationExpired = %q, want the value documented in rest-api.md §7.8 "+
			"and data-classification.md §1.9", codeReservationExpired)
	}
}

// --- MYR-342: the owner's ride-sharing pause ------------------------------

// TestSweep_PausedVehicleIsHeldNotClaimed is the third and last enforcement
// layer. The first two (ride-request create, owner accept) refuse a paused car
// at request time; this one covers the reservation that was already accepted
// when its owner reached for the switch.
//
// HOLD, NOT EXPIRE, and the distinction is the whole design. The claim is
// IRREVERSIBLE — the latch admits one winner for the row's lifetime — so
// claiming a reservation we then decline to push would burn it permanently and
// leave the rider with a resolved-looking ride that never happened. Holding
// costs nothing: the row stays selectable and the next tick re-decides, so an
// owner who un-pauses inside the lateness window gets the dispatch they meant
// to allow. An owner who never un-pauses does not leave the row pending
// forever either — the lateness ceiling expires it naturally
// (TestSweep_LatenessCeilingAppliesToFreeVehicles covers that path), which is
// why this layer needs no expiry logic of its own.
func TestSweep_PausedVehicleIsHeldNotClaimed(t *testing.T) {
	r := testReservation()
	latch := newLatchStore()
	resStore := &fakeReservationStore{
		due:    []DueReservation{r},
		busy:   map[string]bool{},
		paused: map[string]bool{r.VehicleID: true},
	}
	s, exec := newSweeperHarness(t, latch, resStore, &fakeBus{}, func() time.Time { return testSweepNow }, true)

	s.sweepOnce(context.Background())

	if got := latch.order(); len(got) != 1 || !strings.HasPrefix(got[0], "busy:") {
		t.Fatalf("a paused vehicle must be probed and then HELD — no claim, no record. call order = %v", got)
	}
	if len(exec.calls()) != 0 {
		t.Errorf("a paused vehicle must receive no nav push, got %d", len(exec.calls()))
	}
}

// TestSweep_PauseIsCheckedBeforeTheClaim pins the ORDER against the same
// safety argument the busy probe rests on: both questions must be answered
// BEFORE the irreversible claim, never after.
func TestSweep_PauseIsCheckedBeforeTheClaim(t *testing.T) {
	r := testReservation()
	latch := newLatchStore()
	resStore := &fakeReservationStore{
		due:    []DueReservation{r},
		busy:   map[string]bool{},
		paused: map[string]bool{r.VehicleID: true},
	}
	s, _ := newSweeperHarness(t, latch, resStore, &fakeBus{}, func() time.Time { return testSweepNow }, true)

	s.sweepOnce(context.Background())

	if resStore.pauseCount() == 0 {
		t.Fatal("the pause must be probed at all")
	}
	for _, call := range latch.order() {
		if strings.HasPrefix(call, "claim:") {
			t.Fatalf("the claim ran despite a paused vehicle: %v", latch.order())
		}
	}
}

// TestSweep_PauseProbeErrorHoldsRatherThanBurning mirrors the busy probe's
// unknown-state rule. We cannot tell whether pushing would dial a car its owner
// has withdrawn, and a held reservation is recoverable where a burnt claim is
// not — so an unreadable pause state holds. It does NOT dispatch: this is the
// one place in the feature that fails CLOSED, and it can afford to because the
// lateness ceiling still resolves the row honestly. (Contrast the accept path,
// which fails OPEN on an unreadable vehicle, because there a refusal strands a
// human waiting on an answer.)
func TestSweep_PauseProbeErrorHoldsRatherThanBurning(t *testing.T) {
	latch := newLatchStore()
	resStore := &fakeReservationStore{
		due:      []DueReservation{testReservation()},
		busy:     map[string]bool{},
		pauseErr: errors.New("db down"),
	}
	s, exec := newSweeperHarness(t, latch, resStore, &fakeBus{}, func() time.Time { return testSweepNow }, true)

	s.sweepOnce(context.Background())

	for _, call := range latch.order() {
		if strings.HasPrefix(call, "claim:") {
			t.Fatalf("an unreadable pause state must not be claimed: %v", latch.order())
		}
	}
	if len(exec.calls()) != 0 {
		t.Errorf("an unreadable pause state must produce no push, got %d", len(exec.calls()))
	}
}

// TestSweep_EnabledVehicleStillDispatches is the counter-assertion: the new
// probe must not become a blanket hold.
func TestSweep_EnabledVehicleStillDispatches(t *testing.T) {
	r := testReservation()
	latch := newLatchStore()
	resStore := &fakeReservationStore{due: []DueReservation{r}, busy: map[string]bool{}}
	s, exec := newSweeperHarness(t, latch, resStore, &fakeBus{}, func() time.Time { return testSweepNow }, true)

	s.sweepOnce(context.Background())

	if len(exec.calls()) != 1 {
		t.Fatalf("an un-paused vehicle must still be dispatched, got %d pushes", len(exec.calls()))
	}
}
