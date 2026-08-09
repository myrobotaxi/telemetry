package dispatch

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/commands"
	"github.com/myrobotaxi/telemetry/internal/events"
)

// The share values the two fixtures produce. They are DELIBERATELY different
// coordinates: an ordering bug between the legs is only visible when the
// pickup and the dropoff disagree, which is exactly what the MYR-526 ride had
// (Go Fish Poke → 3520 Wheeler St) and exactly what a same-endpoint fixture
// would hide.
const (
	pickupShare  = "37.7955,-122.3937" // testEvent().Pickup
	dropoffShare = "40.7128,-74.006"   // testStartedEvent().Dropoff
)

// gatedExecutor parks the ONE call whose share value is holdValue until
// release is closed (or its ctx dies), and records every call that actually
// COMPLETED, in completion order. Completion order is what matters here: the
// car's navigation destination is last-write-wins, so the last share to land is
// the one the dash ends up showing.
type gatedExecutor struct {
	mu        sync.Mutex
	completed []string

	holdValue   string
	enterOnce   sync.Once
	entered     chan struct{} // closed when the held call is parked
	releaseOnce sync.Once
	release     chan struct{} // closed to let the held call finish
}

// letGo releases the parked call. Safe to call more than once, so a test can
// both release at the point it means to and defer a release that guarantees no
// goroutine is left parked when an assertion fails early.
func (g *gatedExecutor) letGo() { g.releaseOnce.Do(func() { close(g.release) }) }

func newGatedExecutor(holdValue string) *gatedExecutor {
	return &gatedExecutor{
		holdValue: holdValue,
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
	}
}

func (g *gatedExecutor) Execute(ctx context.Context, req commands.Request) (commands.Result, error) {
	value, _ := req.Params["value"].(string)
	if value == g.holdValue {
		g.enterOnce.Do(func() { close(g.entered) })
		select {
		case <-g.release:
		case <-ctx.Done():
			// Stopped before reaching the car — record nothing.
			return commands.Result{}, ctx.Err()
		}
	}
	g.mu.Lock()
	g.completed = append(g.completed, value)
	g.mu.Unlock()
	return commands.Result{Command: req.Command, Applied: true}, nil
}

// last returns the share value of the last call to reach the car, i.e. the
// destination the dash is left on.
func (g *gatedExecutor) last() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.completed) == 0 {
		return ""
	}
	return g.completed[len(g.completed)-1]
}

func (g *gatedExecutor) all() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.completed...)
}

// TestNavLeg_StalledPickupCannotLandAfterDropoff reproduces MYR-526 (live
// 2026-08-09, ride cfb19272…).
//
// The owner accepts with the car already at the kerb and ASLEEP, so leg 1's
// pickup share stalls inside the executor's wake ladder. 12 seconds later the
// rider taps Start and leg 2 fires. Before the fix the two pushes raced: the
// dropoff share reached the car first, the stalled pickup share landed on top
// of it, and the dash sat at the pickup showing "At Destination / End Trip"
// while BOTH legs recorded `sent` — nothing in the row, the app, or the audit
// line disagreed with the car.
//
// The assertion is on the LAST share to reach the car, because that is the one
// the vehicle keeps.
func TestNavLeg_StalledPickupCannotLandAfterDropoff(t *testing.T) {
	exec := newGatedExecutor(pickupShare)
	st := &fakeStore{claimed: true, dropClaimed: true}
	d := newTestDispatcher(exec, st, Config{Enabled: true, MaxRetries: 0})
	defer exec.letGo()

	// Leg 1: the pickup push starts and stalls mid-flight.
	pickupDone := make(chan struct{})
	go func() {
		defer close(pickupDone)
		d.process(context.Background(), testEvent())
	}()
	<-exec.entered

	// Leg 2: the rider taps Start while leg 1 is still in flight.
	dropoffDone := make(chan struct{})
	go func() {
		defer close(dropoffDone)
		d.processDropoff(context.Background(), testStartedEvent())
	}()

	waitClosed(t, dropoffDone, "leg 2 (dropoff) never finished")
	exec.letGo()
	waitClosed(t, pickupDone, "leg 1 (pickup) never finished")

	if got := exec.last(); got != dropoffShare {
		t.Errorf("last share to reach the car = %q, want the DROPOFF %q (calls in completion order: %v)",
			got, dropoffShare, exec.all())
	}
}

// TestNavLeg_SupersededPickupIsRecordedHonestly proves the leg the sequencer
// stops does not masquerade as a Tesla failure or a bare cancellation: it is
// recorded skipped / nav_superseded, so the row and the audit line say what
// actually happened. Never a silent drop.
func TestNavLeg_SupersededPickupIsRecordedHonestly(t *testing.T) {
	exec := newGatedExecutor(pickupShare)
	st := &fakeStore{claimed: true, dropClaimed: true}
	d := newTestDispatcher(exec, st, Config{Enabled: true, MaxRetries: 0})
	defer exec.letGo()

	pickupDone := make(chan struct{})
	go func() {
		defer close(pickupDone)
		d.process(context.Background(), testEvent())
	}()
	<-exec.entered

	dropoffDone := make(chan struct{})
	go func() {
		defer close(dropoffDone)
		d.processDropoff(context.Background(), testStartedEvent())
	}()
	waitClosed(t, dropoffDone, "leg 2 (dropoff) never finished")
	exec.letGo()
	waitClosed(t, pickupDone, "leg 1 (pickup) never finished")

	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.recorded) != 1 || st.recorded[0].status != OutcomeSkipped || st.recorded[0].code != codeNavSuperseded {
		t.Errorf("leg-1 outcome = %+v, want one {skipped, %s}", st.recorded, codeNavSuperseded)
	}
	if len(st.dropRecorded) != 1 || st.dropRecorded[0].status != OutcomeSent {
		t.Errorf("leg-2 outcome = %+v, want one {sent}", st.dropRecorded)
	}
}

// TestNavLeg_PickupAfterDropoffIsRefused covers the other order: leg 2 has
// already reached the car and a leg-1 push for the SAME ride arrives late (a
// re-delivered ride.accepted, or the reservation sweeper firing behind a ride
// that already started). It must never dial Tesla — pushing the pickup would
// walk the dash backwards off the drop-off.
func TestNavLeg_PickupAfterDropoffIsRefused(t *testing.T) {
	exec := &fakeExecutor{errs: []error{nil}}
	st := &fakeStore{claimed: true, dropClaimed: true}
	d := newTestDispatcher(exec, st, Config{Enabled: true, MaxRetries: 0})

	d.processDropoff(context.Background(), testStartedEvent())
	d.runClaimedLeg(context.Background(), d.pickupLeg(
		testEvent().RideRequestID, testEvent().VehicleID, testEvent().OwnerID, testEvent().Pickup))

	if len(exec.calls) != 1 || exec.calls[0].Params["value"] != dropoffShare {
		t.Fatalf("executor calls = %+v, want exactly the dropoff share %q", exec.calls, dropoffShare)
	}
	if len(st.recorded) != 1 || st.recorded[0].status != OutcomeSkipped || st.recorded[0].code != codeNavSuperseded {
		t.Errorf("late leg-1 outcome = %+v, want one {skipped, %s}", st.recorded, codeNavSuperseded)
	}
}

// TestNavLeg_NextRidePickupIsNotSuperseded pins that supersession is keyed by
// RIDE and not by vehicle. The previous ride's dropoff is the last thing this
// car was told; the NEXT ride's pickup is a lower leg ordinal but a different
// ride, and refusing it would leave every car after its first ride stuck
// pointing at the last drop-off.
func TestNavLeg_NextRidePickupIsNotSuperseded(t *testing.T) {
	exec := &fakeExecutor{errs: []error{nil}}
	st := &fakeStore{claimed: true, dropClaimed: true}
	d := newTestDispatcher(exec, st, Config{Enabled: true, MaxRetries: 0})

	d.processDropoff(context.Background(), testStartedEvent())

	next := events.RidePlace{Latitude: 34.0522, Longitude: -118.2437, Label: "Union Station"}
	d.runClaimedLeg(context.Background(), d.pickupLeg("cride999", testEvent().VehicleID, "cowner789", next))

	if len(exec.calls) != 2 {
		t.Fatalf("executor calls = %d, want 2 (previous dropoff, then the next ride's pickup)", len(exec.calls))
	}
	if got := exec.calls[1].Params["value"]; got != "34.0522,-118.2437" {
		t.Errorf("next ride's pickup share = %v, want the new pickup", got)
	}
	if len(st.recorded) != 1 || st.recorded[0].status != OutcomeSent {
		t.Errorf("next ride's leg-1 outcome = %+v, want one {sent}", st.recorded)
	}
}

// TestNavLeg_RepeatedStartPushesOnce proves repeated /start deliveries stay
// idempotent with the sequencer in place: the leg-2 claim latch still admits
// exactly one push, and the second delivery neither dials Tesla nor records a
// second outcome.
func TestNavLeg_RepeatedStartPushesOnce(t *testing.T) {
	exec := &fakeExecutor{errs: []error{nil}}
	st := &fakeStore{dropClaimed: true}
	d := newTestDispatcher(exec, st, Config{Enabled: true, MaxRetries: 0})

	for i := 0; i < 3; i++ {
		d.processDropoff(context.Background(), testStartedEvent())
	}

	if len(exec.calls) != 1 {
		t.Errorf("executor calls = %d across three starts, want 1", len(exec.calls))
	}
	if len(st.dropRecorded) != 1 {
		t.Errorf("dropRecorded = %d across three starts, want 1", len(st.dropRecorded))
	}
}

// TestNavSequencer_SerializesPerVehicle proves two pushes to ONE car never
// overlap (the property that makes completion order equal dispatch order), and
// that pushes to DIFFERENT cars still run concurrently — the gate must not
// serialize the whole fleet behind one asleep vehicle.
func TestNavSequencer_SerializesPerVehicle(t *testing.T) {
	s := newNavSequencer()
	ctx := context.Background()

	h1, err := s.acquire(ctx, "cveh456", "crideA", legOrderPickup, nil)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// A different vehicle is unaffected.
	other, err := s.acquire(ctx, "cvehOTHER", "crideB", legOrderPickup, nil)
	if err != nil {
		t.Fatalf("other-vehicle acquire must not block: %v", err)
	}
	other.Release()

	// The same vehicle waits, and gives up when its ctx dies rather than
	// blocking past its deadline.
	waitCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := s.acquire(waitCtx, "cveh456", "crideB", legOrderPickup, nil); err == nil {
		t.Error("same-vehicle acquire succeeded while another push held the gate")
	}

	h1.Release()

	h2, err := s.acquire(ctx, "cveh456", "crideB", legOrderPickup, nil)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	h2.Release()
}

// TestNavSequencer_RetainsThenPrunesTheHighWaterMark pins both halves of the
// retention rule: an idle vehicle's leg high-water mark SURVIVES the release of
// its last hold (that is when a stale lower leg turns up, and forgetting the
// mark is what would let it through), and it is eventually reclaimed so the map
// tracks the fleet rather than the whole history.
func TestNavSequencer_RetainsThenPrunesTheHighWaterMark(t *testing.T) {
	base := time.Date(2026, 8, 9, 21, 14, 0, 0, time.UTC)
	clock := base
	s := newNavSequencer()
	s.now = func() time.Time { return clock }
	ctx := context.Background()

	h, err := s.acquire(ctx, "cveh456", "crideA", legOrderDropoff, nil)
	if err != nil {
		t.Fatalf("acquire dropoff: %v", err)
	}
	h.Release()

	// A stale leg 1 for the same ride, arriving after everything went idle, is
	// still refused.
	clock = base.Add(2 * time.Minute)
	if _, err := s.acquire(ctx, "cveh456", "crideA", legOrderPickup, nil); err == nil {
		t.Error("a stale pickup after the dropoff was allowed through once the slot went idle")
	}

	// Long after the ride, the slot is reclaimed.
	clock = base.Add(navSlotTTL + time.Hour)
	if _, err := s.acquire(ctx, "cvehOTHER", "crideZ", legOrderPickup, nil); err != nil {
		t.Fatalf("acquire that drives the prune: %v", err)
	}
	s.mu.Lock()
	_, stillThere := s.slots["cveh456"]
	s.mu.Unlock()
	if stillThere {
		t.Error("an idle vehicle slot outlived the retention window")
	}
}

// waitClosed fails the test rather than hanging the suite when a leg never
// finishes.
func waitClosed(t *testing.T, done <-chan struct{}, msg string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal(msg)
	}
}
