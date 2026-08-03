package dispatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/commands"
	"github.com/myrobotaxi/telemetry/internal/events"
)

// --- fakes -----------------------------------------------------------------

// latchStore models the DATABASE, not a per-call script. ClaimDispatch and
// ClaimReservationDispatch are atomic and the FIRST caller for a ride id wins,
// forever; the reservation claim additionally refuses a ride that has left
// `accepted`, exactly as its SQL `status = 'accepted'` conjunct does. Those are
// the properties scheduled dispatch inherits, so the fake has to reproduce them
// faithfully or the concurrency tests prove nothing. One instance is shared by
// every sweeper in a test, exactly as one database is shared by every server
// process in production.
type latchStore struct {
	mu        sync.Mutex
	claimed   map[string]bool
	moved     map[string]bool // ride no longer `accepted` (cancelled / picked-up)
	claims    int             // total claim ATTEMPTS
	won       int             // attempts that won
	recorded  []recordCall
	claimErr  error
	callOrder []string // interleaving of busy/claim/record, for ordering proofs

	// beforeClaim runs immediately before a reservation claim decides, with no
	// lock held. It models a concurrent lifecycle write landing in the
	// select→claim window.
	beforeClaim func(rideID string)
}

func newLatchStore() *latchStore {
	return &latchStore{claimed: make(map[string]bool), moved: make(map[string]bool)}
}

func (s *latchStore) ClaimDispatch(_ context.Context, rideID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claimLocked(rideID)
}

// ClaimReservationDispatch is the sweeper's claim: same latch, plus the
// status re-check.
func (s *latchStore) ClaimReservationDispatch(_ context.Context, rideID string) (bool, error) {
	if s.beforeClaim != nil {
		s.beforeClaim(rideID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callOrder = append(s.callOrder, "claim:"+rideID)
	if s.moved[rideID] {
		// The guarded UPDATE matches no row: the ride is no longer accepted.
		s.claims++
		return false, nil
	}
	return s.claimLocked(rideID)
}

func (s *latchStore) claimLocked(rideID string) (bool, error) {
	s.claims++
	if s.claimErr != nil {
		return false, s.claimErr
	}
	if s.claimed[rideID] {
		return false, nil
	}
	s.claimed[rideID] = true
	s.won++
	return true, nil
}

// moveOn takes a ride out of `accepted` (a rider cancel, or an owner marking
// picked-up) so any later claim for it must lose.
func (s *latchStore) moveOn(rideID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.moved[rideID] = true
}

func (s *latchStore) RecordDispatchOutcome(_ context.Context, rideID string, status Outcome, errCode *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rc := recordCall{status: status}
	if errCode != nil {
		rc.code = *errCode
	}
	s.recorded = append(s.recorded, rc)
	s.callOrder = append(s.callOrder, "record:"+rideID)
	return nil
}

func (s *latchStore) ClaimDropoffDispatch(context.Context, string) (bool, error) { return false, nil }

func (s *latchStore) RecordDropoffDispatchOutcome(context.Context, string, Outcome, *string) error {
	return nil
}

func (s *latchStore) snapshot() (claims, won int, recorded []recordCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claims, s.won, append([]recordCall(nil), s.recorded...)
}

func (s *latchStore) order() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.callOrder...)
}

// fakeReservationStore serves a fixed due list and a per-vehicle busy answer.
// The claim delegates to the shared latch so a test's sweepers all contend on
// one "database".
type fakeReservationStore struct {
	mu                sync.Mutex
	latch             *latchStore
	due               []DueReservation
	busy              map[string]bool
	listErr           error
	busyErr           error
	listCnt           int
	busyCnt           int
	lastNow           time.Time
	lastExpiredBefore time.Time
	lastLimit         int

	// beforeBusy runs at the start of every busy check, with no lock held.
	beforeBusy func(vehicleID string)

	// MYR-342 ride-sharing pause. A vehicle ABSENT from the map is NOT paused,
	// so every pre-existing test keeps its meaning without being touched —
	// which is also the production default (the store COALESCEs a missing
	// control-state row to enabled).
	paused map[string]bool

	// stateErr/stateCnt cover the SINGLE vehicle read that serves both the
	// service arm and the pause arm (MYR-372 collapsed the two probes into
	// one). An unreadable vehicle is therefore one condition, not two — which
	// is exactly what production now sees.
	stateErr error
	stateCnt int

	// MYR-372 vehicle service state at the due instant. A vehicle ABSENT from
	// the map is NOT in service, so every pre-existing test keeps its meaning —
	// and that matches production, where a parked/driving/charging car passes.
	//
	// There is deliberately no `offline` knob: the sweeper does not ask about
	// `offline` at all (holdIfVehicleBlocked explains why), so a fake that
	// could express it would be modelling a question production never poses.
	inService map[string]bool

	// MYR-369 rider ride capability. A (rider|vehicle) pair ABSENT from the
	// map IS permitted, so every pre-existing test keeps its meaning — and
	// that matches production for the common case, where the rider owns the
	// car and the grant table is never consulted.
	ungranted map[string]bool
	grantErr  error
	grantCnt  int
}

func (f *fakeReservationStore) ListDueReservations(
	_ context.Context,
	now, expiredBefore time.Time,
	limit int,
) ([]DueReservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCnt++
	f.lastNow = now
	f.lastExpiredBefore = expiredBefore
	f.lastLimit = limit
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]DueReservation(nil), f.due...), nil
}

func (f *fakeReservationStore) VehicleHasActiveInstantRide(_ context.Context, vehicleID string) (bool, error) {
	if f.beforeBusy != nil {
		f.beforeBusy(vehicleID)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.busyCnt++
	if f.latch != nil {
		f.latch.mu.Lock()
		f.latch.callOrder = append(f.latch.callOrder, "busy:"+vehicleID)
		f.latch.mu.Unlock()
	}
	if f.busyErr != nil {
		return false, f.busyErr
	}
	return f.busy[vehicleID], nil
}

// VehicleDispatchState serves both the MYR-372 service arm and the MYR-342
// pause arm from one call, as production does.
//
// Deliberately NOT recorded in latch.callOrder, matching the grant probe:
// callOrder pins the busy→claim adjacency, and the other probes assert their
// before-the-claim position by the ABSENCE of a claim.
func (f *fakeReservationStore) VehicleDispatchState(
	_ context.Context,
	vehicleID string,
) (VehicleDispatchState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stateCnt++
	if f.stateErr != nil {
		return VehicleDispatchState{}, f.stateErr
	}
	return VehicleDispatchState{
		InService:        f.inService[vehicleID],
		RideShareEnabled: !f.paused[vehicleID],
	}, nil
}

func (f *fakeReservationStore) stateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stateCnt
}

func (f *fakeReservationStore) setInService(vehicleID string, v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.inService == nil {
		f.inService = map[string]bool{}
	}
	f.inService[vehicleID] = v
}

func (f *fakeReservationStore) RiderMayRequestRides(_ context.Context, riderID, vehicleID string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.grantCnt++
	if f.grantErr != nil {
		return false, f.grantErr
	}
	return !f.ungranted[riderID+"|"+vehicleID], nil
}

func (f *fakeReservationStore) grantCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.grantCnt
}

// pauseCount is an alias for stateCount: since MYR-372 the pause arm and the
// service arm are answered by the same read, so "was the pause probed" and
// "was the vehicle probed" are one question. Kept under both names so the
// MYR-342 tests still read in their own vocabulary.
func (f *fakeReservationStore) pauseCount() int { return f.stateCount() }

func (f *fakeReservationStore) ClaimReservationDispatch(ctx context.Context, rideID string) (bool, error) {
	return f.latch.ClaimReservationDispatch(ctx, rideID)
}

func (f *fakeReservationStore) setBusy(vehicleID string, busy bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.busy[vehicleID] = busy
}

// syncExecutor is a race-safe fakeExecutor. The sweeper now runs its
// reservations on real concurrent workers, so the recorder has to be safe under
// -race; it can also BLOCK a chosen vehicle's push, which is how the
// pool-isolation test saturates the sweeper.
type syncExecutor struct {
	mu    sync.Mutex
	reqs  []commands.Request
	err   error
	block chan struct{} // non-nil: every call waits on it before returning
}

func (e *syncExecutor) Execute(ctx context.Context, req commands.Request) (commands.Result, error) {
	e.mu.Lock()
	e.reqs = append(e.reqs, req)
	block, err := e.block, e.err
	e.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return commands.Result{}, ctx.Err()
		}
	}
	if err != nil {
		return commands.Result{}, err
	}
	return commands.Result{Command: req.Command, Applied: true}, nil
}

func (e *syncExecutor) calls() []commands.Request {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]commands.Request(nil), e.reqs...)
}

// fakeBus records published events. Subscribe/Unsubscribe/Close are unused by
// the sweeper (it only publishes).
type fakeBus struct {
	mu         sync.Mutex
	published  []events.Event
	publishErr error
}

func (b *fakeBus) Publish(_ context.Context, evt events.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.publishErr != nil {
		return b.publishErr
	}
	b.published = append(b.published, evt)
	return nil
}

func (b *fakeBus) Subscribe(events.Topic, events.Handler) (events.Subscription, error) {
	return events.Subscription{}, nil
}
func (b *fakeBus) Unsubscribe(events.Subscription) error { return nil }
func (b *fakeBus) Close(context.Context) error           { return nil }

func (b *fakeBus) dueEvents() []events.RideDueEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []events.RideDueEvent
	for _, e := range b.published {
		if ev, ok := e.Payload.(events.RideDueEvent); ok {
			out = append(out, ev)
		}
	}
	return out
}

// --- harness ---------------------------------------------------------------

var (
	testScheduledFor = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	testSweepNow     = testScheduledFor.Add(10 * time.Second)
	// testMaxLateness mirrors the production ceiling the harness configures.
	testMaxLateness = 30 * time.Minute
)

func testReservation() DueReservation {
	return DueReservation{
		RideRequestID: "cride1234567890",
		VehicleID:     "cveh1234567890",
		RiderID:       "crider1234567890",
		OwnerID:       "cowner1234567890",
		Pickup:        events.RidePlace{Latitude: 37.7955, Longitude: -122.3937, Label: "Home"},
		ScheduledFor:  testScheduledFor,
	}
}

// newSweeperHarness builds a sweeper over a real Dispatcher (so the reused
// runClaimedLeg machinery is genuinely exercised) with a controllable clock.
// It wires the reservation store's claim onto the shared latch, mirroring
// production where both go to the same row.
func newSweeperHarness(
	t *testing.T,
	latch *latchStore,
	resStore *fakeReservationStore,
	bus events.Bus,
	now func() time.Time,
	dispatchEnabled bool,
) (*ReservationSweeper, *syncExecutor) {
	t.Helper()
	resStore.latch = latch
	exec := &syncExecutor{}
	d := New(
		&fakeVehicleResolver{vin: "5YJ3E1EA7KF000001"},
		&fakeTokenSource{token: "tok"},
		exec,
		latch,
		Config{Enabled: dispatchEnabled, MaxRetries: 0, Backoff: time.Millisecond},
		nil,
	)
	s := NewReservationSweeper(d, resStore, bus, ReservationConfig{
		Enabled:      true,
		Interval:     time.Millisecond,
		MaxLateness:  testMaxLateness,
		SweepTimeout: 5 * time.Second,
	}, nil).withClock(now)
	return s, exec
}

// --- accept guard ----------------------------------------------------------

// TestProcess_ScheduledAcceptDefersDispatch is the MYR-179 accept guard: a
// scheduled accept must leave the row COMPLETELY untouched — no claim, no
// outcome, no Tesla call — because the latch being unclaimed is exactly what
// makes the sweeper able to find it later. Recording `skipped` here would both
// lie about the ride and latch it out of the sweep.
func TestProcess_ScheduledAcceptDefersDispatch(t *testing.T) {
	latch := newLatchStore()
	exec := &fakeExecutor{}
	d := New(
		&fakeVehicleResolver{vin: "5YJ3E1EA7KF000001"},
		&fakeTokenSource{token: "tok"},
		exec,
		latch,
		Config{Enabled: true},
		nil,
	)

	ev := testEvent()
	sched := testScheduledFor
	ev.ScheduledFor = &sched
	d.process(context.Background(), ev)

	claims, _, recorded := latch.snapshot()
	if claims != 0 {
		t.Errorf("claim attempts = %d, want 0 (a scheduled accept must not touch the latch)", claims)
	}
	if len(recorded) != 0 {
		t.Errorf("recorded outcomes = %v, want none (absent = pending)", recorded)
	}
	if len(exec.calls) != 0 {
		t.Errorf("Tesla calls = %d, want 0 (nav must not fire at accept time)", len(exec.calls))
	}
}

// TestProcess_InstantAcceptStillDispatches is the regression guard: the accept
// deferral must be scoped to reservations only. An instant ride keeps
// dispatching synchronously on accept exactly as it did before MYR-179 — and
// through the UNGUARDED ClaimDispatch, so its behaviour is byte-identical to
// MYR-176.
func TestProcess_InstantAcceptStillDispatches(t *testing.T) {
	latch := newLatchStore()
	exec := &fakeExecutor{}
	d := New(
		&fakeVehicleResolver{vin: "5YJ3E1EA7KF000001"},
		&fakeTokenSource{token: "tok"},
		exec,
		latch,
		Config{Enabled: true},
		nil,
	)

	ev := testEvent()
	ev.ScheduledFor = nil
	d.process(context.Background(), ev)

	_, won, recorded := latch.snapshot()
	if won != 1 {
		t.Errorf("claims won = %d, want 1", won)
	}
	if len(recorded) != 1 || recorded[0].status != OutcomeSent {
		t.Errorf("recorded = %v, want one sent outcome", recorded)
	}
	if len(exec.calls) != 1 {
		t.Errorf("Tesla calls = %d, want 1", len(exec.calls))
	}
}

// --- sweeper: the happy path ----------------------------------------------

// TestSweep_DispatchesDueReservation walks the whole reservation path: due row
// → busy check → claim → the same nav push an instant accept makes → ride.due.
func TestSweep_DispatchesDueReservation(t *testing.T) {
	latch := newLatchStore()
	resStore := &fakeReservationStore{due: []DueReservation{testReservation()}, busy: map[string]bool{}}
	bus := &fakeBus{}
	s, exec := newSweeperHarness(t, latch, resStore, bus, func() time.Time { return testSweepNow }, true)

	res := s.sweepOnce(context.Background())

	if res.due != 1 || res.dispatched != 1 || res.held != 0 || res.expired != 0 {
		t.Errorf("sweep = %+v, want one due reservation dispatched", res)
	}
	_, won, recorded := latch.snapshot()
	if won != 1 {
		t.Errorf("claims won = %d, want 1", won)
	}
	if len(recorded) != 1 || recorded[0].status != OutcomeSent {
		t.Errorf("recorded = %v, want one sent outcome", recorded)
	}
	calls := exec.calls()
	if len(calls) != 1 {
		t.Fatalf("Tesla calls = %d, want 1", len(calls))
	}
	// The pushed destination must be the reservation's PICKUP.
	if got := calls[0].Params["value"]; got != "37.7955,-122.3937" {
		t.Errorf("pushed destination = %v, want the reservation pickup", got)
	}

	due := bus.dueEvents()
	if len(due) != 1 {
		t.Fatalf("ride.due events = %d, want exactly 1", len(due))
	}
	if due[0].RideRequestID != testReservation().RideRequestID ||
		!due[0].ScheduledFor.Equal(testScheduledFor) ||
		!due[0].DueAt.Equal(testSweepNow) {
		t.Errorf("ride.due = %+v, want the reservation's ids and instants", due[0])
	}
}

// TestSweep_SecondSweepDoesNotRedispatch proves ticking again is harmless once
// the row is claimed — in production the due query would no longer return it,
// but even if it did the latch is the backstop.
func TestSweep_SecondSweepDoesNotRedispatch(t *testing.T) {
	latch := newLatchStore()
	resStore := &fakeReservationStore{due: []DueReservation{testReservation()}, busy: map[string]bool{}}
	bus := &fakeBus{}
	s, exec := newSweeperHarness(t, latch, resStore, bus, func() time.Time { return testSweepNow }, true)

	s.sweepOnce(context.Background())
	second := s.sweepOnce(context.Background())

	if second.dispatched != 0 || second.lost != 1 {
		t.Errorf("second sweep = %+v, want the claim lost and nothing dispatched", second)
	}
	if got := len(exec.calls()); got != 1 {
		t.Errorf("Tesla calls = %d, want 1 across both sweeps", got)
	}
	if got := len(bus.dueEvents()); got != 1 {
		t.Errorf("ride.due events = %d, want exactly 1 per due ride", got)
	}
}

// TestSweep_ConcurrentSweepersDispatchExactlyOnce is the multi-process claim
// race: two servers sweeping the same reservation simultaneously must resolve
// to ONE nav push and ONE ride.due. This is the property that lets the sweeper
// run on every replica without coordination.
func TestSweep_ConcurrentSweepersDispatchExactlyOnce(t *testing.T) {
	latch := newLatchStore() // the shared "database"
	bus := &fakeBus{}
	now := func() time.Time { return testSweepNow }

	const sweepers = 2
	svs := make([]*ReservationSweeper, sweepers)
	execs := make([]*syncExecutor, sweepers)
	for i := range svs {
		resStore := &fakeReservationStore{due: []DueReservation{testReservation()}, busy: map[string]bool{}}
		svs[i], execs[i] = newSweeperHarness(t, latch, resStore, bus, now, true)
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, s := range svs {
		wg.Add(1)
		go func(s *ReservationSweeper) {
			defer wg.Done()
			<-start
			s.sweepOnce(context.Background())
		}(s)
	}
	close(start)
	wg.Wait()

	claims, won, recorded := latch.snapshot()
	if claims != sweepers {
		t.Errorf("claim attempts = %d, want %d (both sweepers tried)", claims, sweepers)
	}
	if won != 1 {
		t.Errorf("claims won = %d, want exactly 1 winner", won)
	}
	if len(recorded) != 1 {
		t.Errorf("recorded outcomes = %v, want exactly 1", recorded)
	}

	total := 0
	for _, e := range execs {
		total += len(e.calls())
	}
	if total != 1 {
		t.Errorf("Tesla calls across sweepers = %d, want exactly 1", total)
	}
	if got := len(bus.dueEvents()); got != 1 {
		t.Errorf("ride.due events = %d, want exactly 1", got)
	}
}

// --- sweeper: the vehicle-busy hold ---------------------------------------

// TestSweep_BusyVehicleHoldsThenFailsPastWindow is the busy-hold policy end to
// end on one reservation: while the car is mid-ride the reservation is held
// untouched and retried; once it has waited past the lateness ceiling it is
// claimed and failed HONESTLY, with no nav push and no ride.due (the car never
// came).
func TestSweep_BusyVehicleHoldsThenFailsPastWindow(t *testing.T) {
	r := testReservation()
	latch := newLatchStore()
	resStore := &fakeReservationStore{due: []DueReservation{r}, busy: map[string]bool{r.VehicleID: true}}
	bus := &fakeBus{}

	var (
		clockMu sync.Mutex
		clock   = testSweepNow
	)
	readClock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}
	setClock := func(v time.Time) {
		clockMu.Lock()
		defer clockMu.Unlock()
		clock = v
	}
	s, exec := newSweeperHarness(t, latch, resStore, bus, readClock, true)

	// Inside the window: hold, and leave the row completely untouched so the
	// next tick can still dispatch it if the car frees up.
	for i := range 3 {
		res := s.sweepOnce(context.Background())
		if res.held != 1 || res.dispatched != 0 || res.expired != 0 {
			t.Fatalf("sweep %d = %+v, want held", i, res)
		}
		setClock(readClock().Add(5 * time.Minute))
	}
	claims, _, recorded := latch.snapshot()
	if claims != 0 {
		t.Errorf("claim attempts during hold = %d, want 0", claims)
	}
	if len(recorded) != 0 {
		t.Errorf("recorded during hold = %v, want none", recorded)
	}

	// Past scheduledFor + the ceiling with the car still busy: stop waiting.
	setClock(r.ScheduledFor.Add(testMaxLateness + time.Second))
	res := s.sweepOnce(context.Background())

	if res.expired != 1 || res.dispatched != 0 {
		t.Errorf("expiry sweep = %+v, want one expired reservation", res)
	}
	_, won, recorded := latch.snapshot()
	if won != 1 {
		t.Errorf("claims won = %d, want 1 (the expiry claim resolves the row)", won)
	}
	if len(recorded) != 1 {
		t.Fatalf("recorded = %v, want exactly one outcome", recorded)
	}
	if recorded[0].status != OutcomeFailed || recorded[0].code != codeReservationExpired {
		t.Errorf("recorded = %+v, want failed/%s", recorded[0], codeReservationExpired)
	}
	if got := len(exec.calls()); got != 0 {
		t.Errorf("Tesla calls = %d, want 0 (an expired reservation is never pushed)", got)
	}
	if got := len(bus.dueEvents()); got != 0 {
		t.Errorf("ride.due events = %d, want 0 (the reservation never dispatched)", got)
	}
}

// TestSweep_BusyVehicleFreesUpBeforeWindowExpires proves the hold is a WAIT,
// not a deferral to failure: the moment the car finishes its ride the held
// reservation dispatches normally.
func TestSweep_BusyVehicleFreesUpBeforeWindowExpires(t *testing.T) {
	r := testReservation()
	latch := newLatchStore()
	resStore := &fakeReservationStore{due: []DueReservation{r}, busy: map[string]bool{r.VehicleID: true}}
	bus := &fakeBus{}
	var (
		clockMu sync.Mutex
		clock   = testSweepNow
	)
	s, exec := newSweeperHarness(t, latch, resStore, bus, func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}, true)

	if res := s.sweepOnce(context.Background()); res.held != 1 {
		t.Fatalf("first sweep = %+v, want held", res)
	}

	resStore.setBusy(r.VehicleID, false)
	clockMu.Lock()
	clock = clock.Add(time.Minute)
	clockMu.Unlock()

	res := s.sweepOnce(context.Background())

	if res.dispatched != 1 {
		t.Errorf("second sweep = %+v, want the reservation dispatched", res)
	}
	if got := len(exec.calls()); got != 1 {
		t.Errorf("Tesla calls = %d, want 1", got)
	}
	if got := len(bus.dueEvents()); got != 1 {
		t.Errorf("ride.due events = %d, want 1", got)
	}
}

// TestSweep_BusyCheckErrorDoesNotClaim: an unknown busy state must never
// claim. A held reservation is recoverable on the next tick; a nav push that
// hijacks a live ride is not.
func TestSweep_BusyCheckErrorDoesNotClaim(t *testing.T) {
	latch := newLatchStore()
	resStore := &fakeReservationStore{
		due:     []DueReservation{testReservation()},
		busy:    map[string]bool{},
		busyErr: errors.New("connection reset"),
	}
	s, exec := newSweeperHarness(t, latch, resStore, &fakeBus{}, func() time.Time { return testSweepNow }, true)

	res := s.sweepOnce(context.Background())

	if res.held != 1 || res.dispatched != 0 {
		t.Errorf("sweep = %+v, want held on an unknown busy state", res)
	}
	claims, _, recorded := latch.snapshot()
	if claims != 0 || len(recorded) != 0 {
		t.Errorf("claims = %d, recorded = %v, want the row untouched", claims, recorded)
	}
	if got := len(exec.calls()); got != 0 {
		t.Errorf("Tesla calls = %d, want 0", got)
	}
}

// --- sweeper: failure isolation + config ----------------------------------

// TestSweep_ListErrorIsSurvivable proves a database blip costs one tick, not
// the reservation: nothing is claimed and the next sweep retries.
func TestSweep_ListErrorIsSurvivable(t *testing.T) {
	latch := newLatchStore()
	resStore := &fakeReservationStore{listErr: errors.New("pool exhausted"), busy: map[string]bool{}}
	s, _ := newSweeperHarness(t, latch, resStore, &fakeBus{}, func() time.Time { return testSweepNow }, true)

	res := s.sweepOnce(context.Background())
	if res.due != 0 || res.dispatched != 0 {
		t.Errorf("sweep = %+v, want an empty pass", res)
	}
	if claims, _, _ := latch.snapshot(); claims != 0 {
		t.Errorf("claim attempts = %d, want 0", claims)
	}
}

// TestSweep_PublishFailureDoesNotBlockDispatch: ride.due is a notification
// hook, the nav push is the contract. A bus failure must not cost the rider
// their ride.
func TestSweep_PublishFailureDoesNotBlockDispatch(t *testing.T) {
	latch := newLatchStore()
	resStore := &fakeReservationStore{due: []DueReservation{testReservation()}, busy: map[string]bool{}}
	bus := &fakeBus{publishErr: errors.New("bus closed")}
	s, exec := newSweeperHarness(t, latch, resStore, bus, func() time.Time { return testSweepNow }, true)

	s.sweepOnce(context.Background())

	if got := len(exec.calls()); got != 1 {
		t.Errorf("Tesla calls = %d, want 1 despite the publish failure", got)
	}
	if _, _, recorded := latch.snapshot(); len(recorded) != 1 || recorded[0].status != OutcomeSent {
		t.Errorf("recorded = %v, want one sent outcome", recorded)
	}
}

// TestSweep_NilBusIsSafe — the seam is optional wiring, not a dependency.
func TestSweep_NilBusIsSafe(t *testing.T) {
	latch := newLatchStore()
	resStore := &fakeReservationStore{due: []DueReservation{testReservation()}, busy: map[string]bool{}}
	s, exec := newSweeperHarness(t, latch, resStore, nil, func() time.Time { return testSweepNow }, true)

	s.sweepOnce(context.Background())

	if got := len(exec.calls()); got != 1 {
		t.Errorf("Tesla calls = %d, want 1", got)
	}
}

// TestReservationSweeper_KillSwitchStopsTheLoop proves
// RESERVATION_DISPATCH_ENABLED=false never sweeps at all — reservations stay
// unclaimed and outcome-absent, so re-enabling picks them up rather than
// having burned them.
func TestReservationSweeper_KillSwitchStopsTheLoop(t *testing.T) {
	latch := newLatchStore()
	resStore := &fakeReservationStore{latch: latch, due: []DueReservation{testReservation()}, busy: map[string]bool{}}
	d := New(&fakeVehicleResolver{vin: "5YJ"}, &fakeTokenSource{token: "t"}, &fakeExecutor{}, latch, Config{Enabled: true}, nil)
	s := NewReservationSweeper(d, resStore, &fakeBus{}, ReservationConfig{
		Enabled:  false,
		Interval: time.Millisecond,
	}, nil)

	done := make(chan struct{})
	go func() {
		s.Run(context.Background()) // must return immediately, NOT block on a ticker
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return with the kill-switch off")
	}

	resStore.mu.Lock()
	listCnt := resStore.listCnt
	resStore.mu.Unlock()
	if listCnt != 0 {
		t.Errorf("due-list queries = %d, want 0 with the sweeper disabled", listCnt)
	}
}

// TestReservationSweeper_RunSweepsOnTick proves the loop actually drives
// sweepOnce and exits cleanly on context cancellation.
func TestReservationSweeper_RunSweepsOnTick(t *testing.T) {
	latch := newLatchStore()
	resStore := &fakeReservationStore{due: []DueReservation{testReservation()}, busy: map[string]bool{}}
	bus := &fakeBus{}
	s, _ := newSweeperHarness(t, latch, resStore, bus, func() time.Time { return testSweepNow }, true)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.Run(ctx)
		close(done)
	}()

	deadline := time.After(3 * time.Second)
	for {
		if _, won, _ := latch.snapshot(); won == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the sweeper never dispatched the due reservation")
		case <-time.After(2 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit on context cancellation")
	}

	// Even across many ticks the latch admits one winner.
	if _, won, recorded := latch.snapshot(); won != 1 || len(recorded) != 1 {
		t.Errorf("won = %d, recorded = %v, want exactly one dispatch across all ticks", won, recorded)
	}
}

// TestReservationConfig_Defaults pins the documented defaults: the 30s cadence,
// the 30-minute lateness ceiling, and the two batch/isolation knobs the review
// round introduced (a modest per-sweep window and the sweeper's OWN small
// worker budget).
func TestReservationConfig_Defaults(t *testing.T) {
	got := ReservationConfig{Enabled: true}.withDefaults()
	if got.Interval != 30*time.Second {
		t.Errorf("Interval = %v, want 30s", got.Interval)
	}
	if got.MaxLateness != 30*time.Minute {
		t.Errorf("MaxLateness = %v, want 30m", got.MaxLateness)
	}
	if got.SweepTimeout <= 0 {
		t.Errorf("SweepTimeout = %v, want a positive default", got.SweepTimeout)
	}
	if got.MaxPerSweep != 25 {
		t.Errorf("MaxPerSweep = %d, want 25 (claims are just-in-time; an unreached "+
			"candidate reappears next tick)", got.MaxPerSweep)
	}
	if got.MaxConcurrent != 2 {
		t.Errorf("MaxConcurrent = %d, want 2 (the sweeper's own budget, not the "+
			"dispatcher's instant pool)", got.MaxConcurrent)
	}
}

// TestSweep_PassesSweeperClockToTheStore proves ONE clock governs the due
// selection, the anti-starvation expiry window, and the Go-side deadline — the
// reason the query takes `now` and `expiredBefore` instead of using NOW().
func TestSweep_PassesSweeperClockToTheStore(t *testing.T) {
	latch := newLatchStore()
	resStore := &fakeReservationStore{busy: map[string]bool{}}
	s, _ := newSweeperHarness(t, latch, resStore, &fakeBus{}, func() time.Time { return testSweepNow }, true)

	s.sweepOnce(context.Background())

	resStore.mu.Lock()
	defer resStore.mu.Unlock()
	if !resStore.lastNow.Equal(testSweepNow) {
		t.Errorf("due query now = %v, want the sweeper clock %v", resStore.lastNow, testSweepNow)
	}
	if want := testSweepNow.Add(-testMaxLateness); !resStore.lastExpiredBefore.Equal(want) {
		t.Errorf("due query expiredBefore = %v, want now - MaxLateness %v", resStore.lastExpiredBefore, want)
	}
	if resStore.lastLimit <= 0 {
		t.Errorf("due query limit = %d, want the configured cap", resStore.lastLimit)
	}
}
