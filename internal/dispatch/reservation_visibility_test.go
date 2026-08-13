package dispatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// MYR-555 — the two things a scheduled ride's dispatch was missing: a frame an
// OPEN APP could see, and a ceiling that still applies once the car has been
// dialled.

// dispatchFrames returns the RideStatusChangedEvents a bus collected.
func dispatchFrames(t *testing.T, bus *fakeBus) []events.RideStatusChangedEvent {
	t.Helper()
	bus.mu.Lock()
	defer bus.mu.Unlock()
	var out []events.RideStatusChangedEvent
	for i := range bus.published {
		if ev, ok := bus.published[i].Payload.(events.RideStatusChangedEvent); ok {
			out = append(out, ev)
		}
	}
	return out
}

// --- the dispatch frame ----------------------------------------------------

// TestSweep_DispatchPublishesTheStatusFrame is the whole of the reported bug:
// a reservation dispatched correctly and the app looking at it learned nothing,
// because `ride.due` has no WS consumer. The summary frame is what the
// broadcaster fans to the parties.
func TestSweep_DispatchPublishesTheStatusFrame(t *testing.T) {
	latch := newLatchStore()
	resStore := &fakeReservationStore{busy: map[string]bool{}}
	bus := &fakeBus{}
	s, _ := newSweeperHarness(t, latch, resStore, bus, func() time.Time { return testSweepNow }, true)

	r := testReservation()
	r.TripVersion = 5
	if got := s.handleDue(context.Background(), &r); got != decisionDispatched {
		t.Fatalf("decision = %v, want dispatched", got)
	}

	frames := dispatchFrames(t, bus)
	if len(frames) != 1 {
		t.Fatalf("published %d ride_status_changed frames, want 1", len(frames))
	}
	f := frames[0]

	// The status does NOT move — the ride was accepted and still is. The frame
	// is a refetch signal, not a transition.
	if f.Status != "accepted" || f.PreviousStatus != "accepted" {
		t.Errorf("status/previous = %q/%q, want accepted/accepted", f.Status, f.PreviousStatus)
	}
	// The marker that keeps the push notifier out of it.
	if !f.DispatchEdit {
		t.Error("DispatchEdit = false, want true — without it the frame re-fires the accept push")
	}
	// MYR-548's carry rule: the ride's CURRENT version, never the wire's zero.
	if f.TripVersion != 5 {
		t.Errorf("TripVersion = %d, want the ride's current 5", f.TripVersion)
	}
	// Addressed to both parties, so the broadcaster can unicast to each.
	if f.RiderID != r.RiderID || f.OwnerID != r.OwnerID {
		t.Errorf("parties = %q/%q, want %q/%q", f.RiderID, f.OwnerID, r.RiderID, r.OwnerID)
	}
	if f.RideRequestID != r.RideRequestID || f.VehicleID != r.VehicleID {
		t.Errorf("ids = %q/%q, want %q/%q", f.RideRequestID, f.VehicleID, r.RideRequestID, r.VehicleID)
	}
}

// TestSweep_NoFrameWithoutASentPush pins the seam rule both publishes share:
// they mean "your car is on the way", so a kill-switched or failed push must
// emit neither. The latch admits one winner, so a false frame could never be
// corrected by a later one.
func TestSweep_NoFrameWithoutASentPush(t *testing.T) {
	tests := []struct {
		name            string
		dispatchEnabled bool
		execErr         error
	}{
		{name: "kill-switched dispatch records skipped", dispatchEnabled: false},
		{name: "the command failed", dispatchEnabled: true, execErr: errors.New("tesla said no")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			latch := newLatchStore()
			resStore := &fakeReservationStore{busy: map[string]bool{}}
			bus := &fakeBus{}
			s, exec := newSweeperHarness(t, latch, resStore, bus, func() time.Time { return testSweepNow }, tt.dispatchEnabled)
			exec.err = tt.execErr

			r := testReservation()
			s.handleDue(context.Background(), &r)

			if frames := dispatchFrames(t, bus); len(frames) != 0 {
				t.Errorf("published %d frames, want 0 — nothing reached the car", len(frames))
			}
		})
	}
}

// TestSweep_ExpiryPublishesNoFrame: the never-dispatched ceiling arm resolves a
// reservation WITHOUT a push, so it must announce nothing either. A frame here
// would tell an open app the car had left.
func TestSweep_ExpiryPublishesNoFrame(t *testing.T) {
	latch := newLatchStore()
	resStore := &fakeReservationStore{busy: map[string]bool{}}
	bus := &fakeBus{}
	late := testScheduledFor.Add(testMaxLateness + time.Minute)
	s, _ := newSweeperHarness(t, latch, resStore, bus, func() time.Time { return late }, true)

	r := testReservation()
	if got := s.handleDue(context.Background(), &r); got != decisionExpired {
		t.Fatalf("decision = %v, want expired", got)
	}
	bus.mu.Lock()
	defer bus.mu.Unlock()
	if len(bus.published) != 0 {
		t.Errorf("published %d events on expiry, want 0", len(bus.published))
	}
}

// --- the lapsed-dispatch ceiling -------------------------------------------

// TestSweep_LapsedDispatchExpires is prod c92b2c86: dispatched at 21:40:24Z for
// a 21:45Z pickup, never driven, still `accepted` hours later. The first pass
// can never see it — its `dispatched_at IS NULL` conjunct drops the row the
// moment it is claimed — so the second pass is what resolves it.
func TestSweep_LapsedDispatchExpires(t *testing.T) {
	latch := newLatchStore()
	resStore := &fakeReservationStore{
		busy: map[string]bool{},
		lapsed: []LapsedReservation{{
			RideRequestID: "cride1234567890",
			VehicleID:     "cveh1234567890",
			ScheduledFor:  testScheduledFor,
		}},
	}
	ender := &fakeActivityEnder{}
	late := testScheduledFor.Add(testMaxLateness + time.Minute)
	s, _ := newSweeperHarness(t, latch, resStore, &fakeBus{}, func() time.Time { return late }, true)
	s.WithActivityEnder(ender)

	res := s.sweepOnce(context.Background())

	if res.lapsed != 1 {
		t.Errorf("lapsed = %d, want 1", res.lapsed)
	}
	if got := resStore.expiredIDs(); len(got) != 1 || got[0] != "cride1234567890" {
		t.Errorf("expired = %v, want [cride1234567890]", got)
	}
	// The courtesy that matters most here: this rider's card has been counting
	// down since the car was dialled, and nothing else can end it.
	if got := ender.ended(); len(got) != 1 || got[0] != "cride1234567890" {
		t.Errorf("ended activities = %v, want the lapsed ride", got)
	}
}

// TestSweep_LapsedExpiryIsOneWinner pins the self-terminating property. The
// usual latch cannot serve here (`dispatched_at` is already stamped on every row
// in this set), so the resolving statement carries the guarantee — and without
// it endActivities would re-fire every 30 seconds forever.
func TestSweep_LapsedExpiryIsOneWinner(t *testing.T) {
	latch := newLatchStore()
	resStore := &fakeReservationStore{
		busy: map[string]bool{},
		lapsed: []LapsedReservation{{
			RideRequestID: "cride1234567890",
			VehicleID:     "cveh1234567890",
			ScheduledFor:  testScheduledFor,
		}},
	}
	ender := &fakeActivityEnder{}
	late := testScheduledFor.Add(testMaxLateness + time.Minute)
	s, _ := newSweeperHarness(t, latch, resStore, &fakeBus{}, func() time.Time { return late }, true)
	s.WithActivityEnder(ender)

	// Three passes over a fake that keeps re-listing the row — the production
	// SELECT would stop returning it, but the guarantee must not depend on that.
	for range 3 {
		s.sweepOnce(context.Background())
	}

	if got := resStore.expiredIDs(); len(got) != 1 {
		t.Errorf("verdict recorded %d times, want exactly 1: %v", len(got), got)
	}
	if got := ender.ended(); len(got) != 1 {
		t.Errorf("activities ended %d times, want exactly 1", len(got))
	}
}

// TestSweep_LapsedListUsesTheCeilingClock: both passes of a tick derive their
// bounds from the SWEEPER's clock, so no decision can straddle a skew and the
// boundary is exactly testable.
func TestSweep_LapsedListUsesTheCeilingClock(t *testing.T) {
	latch := newLatchStore()
	resStore := &fakeReservationStore{busy: map[string]bool{}}
	s, _ := newSweeperHarness(t, latch, resStore, &fakeBus{}, func() time.Time { return testSweepNow }, true)

	s.sweepOnce(context.Background())

	resStore.mu.Lock()
	defer resStore.mu.Unlock()
	if resStore.lapsedCnt != 1 {
		t.Fatalf("lapsed list calls = %d, want 1 per pass", resStore.lapsedCnt)
	}
	if want := testSweepNow.Add(-testMaxLateness); !resStore.lastLapsed.Equal(want) {
		t.Errorf("lapsedBefore = %v, want now-MaxLateness %v", resStore.lastLapsed, want)
	}
	// The same instant the due pass measures its own ceiling from.
	if !resStore.lastExpiredBefore.Equal(resStore.lastLapsed) {
		t.Errorf("the two passes disagree on the ceiling: %v vs %v",
			resStore.lastExpiredBefore, resStore.lastLapsed)
	}
}

// TestSweep_LapsedListFailureDoesNotKillThePass: the second pass is a courtesy
// over rides that are already hours gone. A database blip in it must not lose
// the dispatch the FIRST pass just performed.
func TestSweep_LapsedListFailureDoesNotKillThePass(t *testing.T) {
	latch := newLatchStore()
	r := testReservation()
	resStore := &fakeReservationStore{
		busy:      map[string]bool{},
		due:       []DueReservation{r},
		lapsedErr: errors.New("database is having a moment"),
	}
	bus := &fakeBus{}
	s, _ := newSweeperHarness(t, latch, resStore, bus, func() time.Time { return testSweepNow }, true)

	res := s.sweepOnce(context.Background())

	if res.dispatched != 1 {
		t.Errorf("dispatched = %d, want 1 — the lapsed failure must not touch the due pass", res.dispatched)
	}
	if res.lapsed != 0 {
		t.Errorf("lapsed = %d, want 0", res.lapsed)
	}
	if frames := dispatchFrames(t, bus); len(frames) != 1 {
		t.Errorf("dispatch frames = %d, want 1", len(frames))
	}
}

// fakeActivityEnder records the rides whose Live Activities were ended. The
// sweeper's ender seam had no test double before MYR-555 because the
// never-dispatched arm's courtesy was only ever asserted through the notifier's
// own suite; the lapsed arm needs it here, since ending the card is the ONLY
// user-visible thing that arm does.
type fakeActivityEnder struct {
	mu   sync.Mutex
	ends []string
}

func (f *fakeActivityEnder) EndForReservationExpiry(_ context.Context, rideRequestID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ends = append(f.ends, rideRequestID)
}

func (f *fakeActivityEnder) ended() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ends...)
}
