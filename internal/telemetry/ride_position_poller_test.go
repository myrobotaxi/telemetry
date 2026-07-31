package telemetry

// MYR-394 ride-position poller tests.
//
// The invariants under test are the ones that make this feature safe rather
// than merely working: a poller is bounded to its ride, it never wakes a
// sleeping car, it yields to live telemetry, the reconcile is the authority
// over the in-memory registry, and shutdown leaks nothing.

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

const (
	ridePollVIN     = "7SAYGDET7TA613795"
	ridePollVehicle = "veh-1"
	ridePollRide    = "ride-1"
)

// --- fakes ---------------------------------------------------------------

// fakeBackfill records every RefreshFromVehicleData call. It is the ONLY Tesla
// seam the poller has, which is also how these tests prove no wake can happen:
// there is no other method to call.
type fakeBackfill struct {
	mu    sync.Mutex
	calls []string // VINs, in order
	err   error
	// gate, when non-nil, is signalled on each call so a test can observe a
	// cycle without racing a timer.
	gate chan struct{}
}

func (f *fakeBackfill) RefreshFromVehicleData(_ context.Context, vin, _ string) error {
	f.mu.Lock()
	f.calls = append(f.calls, vin)
	err := f.err
	gate := f.gate
	f.mu.Unlock()

	if gate != nil {
		select {
		case gate <- struct{}{}:
		default:
		}
	}
	return err
}

func (f *fakeBackfill) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeBackfill) setErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// fakeStreamRecency answers the MYR-300 freshness question the poll yields to.
type fakeStreamRecency struct {
	mu    sync.Mutex
	fresh bool
}

func (f *fakeStreamRecency) LastStreamAt(string) (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return time.Now(), f.fresh
}

func (f *fakeStreamRecency) setFresh(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fresh = v
}

type fakeRideVehicles struct {
	vins map[string]string
	err  error
}

func (f *fakeRideVehicles) ResolveVIN(_ context.Context, vehicleID string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	if vin, ok := f.vins[vehicleID]; ok {
		return vin, nil
	}
	return "", fmt.Errorf("no vehicle %q", vehicleID)
}

type fakeRideLister struct {
	mu      sync.Mutex
	targets []ActiveRideTarget
	err     error
	calls   int
}

func (f *fakeRideLister) ListActiveRideTargets(_ context.Context, _ int) ([]ActiveRideTarget, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	out := make([]ActiveRideTarget, len(f.targets))
	copy(out, f.targets)
	return out, nil
}

func (f *fakeRideLister) set(targets ...ActiveRideTarget) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.targets = targets
}

// --- harness -------------------------------------------------------------

type ridePollHarness struct {
	poller   *RidePositionPoller
	bus      events.Bus
	backfill *fakeBackfill
	streams  *fakeStreamRecency
	lister   *fakeRideLister
}

// newRidePollHarness builds a poller on a real ChannelBus. The interval is
// deliberately tiny so a test observes several cycles in milliseconds; every
// assertion below is on an OBSERVED call, never on elapsed time.
func newRidePollHarness(t *testing.T, cfg RidePositionPollConfig) *ridePollHarness {
	t.Helper()

	bus := events.NewChannelBus(events.BusConfig{BufferSize: 16}, events.NoopBusMetrics{}, discardLogger())
	h := &ridePollHarness{
		bus:      bus,
		backfill: &fakeBackfill{gate: make(chan struct{}, 64)},
		streams:  &fakeStreamRecency{},
		lister:   &fakeRideLister{},
	}
	if cfg.Interval == 0 {
		cfg.Interval = 5 * time.Millisecond
	}
	cfg.Enabled = true

	h.poller = NewRidePositionPoller(
		bus,
		NewRidePositionPollerDeps(
			h.backfill,
			h.streams,
			&fakeRideVehicles{vins: map[string]string{ridePollVehicle: ridePollVIN, "veh-2": "7SAYGDET7TA613796"}},
			&stubVehicleOwner{ownerID: "user-1"},
			&fakeTokenResolver{tok: TeslaToken{AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour)}},
			h.lister,
		),
		cfg,
		discardLogger(),
	)

	if err := h.poller.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(h.poller.Stop)
	return h
}

// awaitPoll waits for the next observed vehicle_data call.
func (h *ridePollHarness) awaitPoll(t *testing.T) bool {
	t.Helper()
	select {
	case <-h.backfill.gate:
		return true
	case <-time.After(2 * time.Second):
		return false
	}
}

// awaitActive waits until the registry holds want entries, so a test never
// races the bus's asynchronous delivery.
func (h *ridePollHarness) awaitActive(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.poller.ActiveCount() == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("active pollers = %d, want %d", h.poller.ActiveCount(), want)
}

func (h *ridePollHarness) publish(t *testing.T, payload events.EventPayload) {
	t.Helper()
	if err := h.bus.Publish(context.Background(), events.NewEvent(payload)); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

func acceptedEvt(scheduled *time.Time) events.RideAcceptedEvent {
	return events.RideAcceptedEvent{
		RideRequestID: ridePollRide,
		VehicleID:     ridePollVehicle,
		RiderID:       "rider-1",
		OwnerID:       "user-1",
		ScheduledFor:  scheduled,
		AcceptedAt:    time.Now(),
	}
}

func statusEvt(status string) events.RideStatusChangedEvent {
	return events.RideStatusChangedEvent{
		RideRequestID: ridePollRide,
		VehicleID:     ridePollVehicle,
		Status:        status,
		UpdatedAt:     time.Now(),
	}
}

// --- tests ---------------------------------------------------------------

// TestRidePoller_StartsOnAccept is the headline behaviour: the moment an owner
// accepts an instant ride, the car's position starts being read — and the FIRST
// read is immediate, because the rider opens the tracking screen right then and
// a 25s wait would leave the reported defect on screen for exactly the window
// it is most looked at.
func TestRidePoller_StartsOnAccept(t *testing.T) {
	h := newRidePollHarness(t, RidePositionPollConfig{Interval: time.Hour})

	h.publish(t, acceptedEvt(nil))
	h.awaitActive(t, 1)

	if !h.awaitPoll(t) {
		t.Fatal("no vehicle_data read after accept; the first cycle must not wait for the interval")
	}
	if got, ok := h.poller.PollingRide(ridePollVehicle); !ok || got != ridePollRide {
		t.Fatalf("PollingRide = (%q, %v), want (%q, true)", got, ok, ridePollRide)
	}
}

// TestRidePoller_IgnoresScheduledAccept guards the rate budget: a reservation
// accepted for next Tuesday must not start polling a car that is parked in a
// driveway with nobody watching. Its moment comes on ride.due.
func TestRidePoller_IgnoresScheduledAccept(t *testing.T) {
	h := newRidePollHarness(t, RidePositionPollConfig{Interval: time.Hour})

	sched := time.Now().Add(48 * time.Hour)
	h.publish(t, acceptedEvt(&sched))

	// Give the bus a real chance to deliver before asserting a negative.
	time.Sleep(50 * time.Millisecond)
	if n := h.poller.ActiveCount(); n != 0 {
		t.Fatalf("active pollers = %d, want 0 for a reservation that is not yet live", n)
	}

	h.publish(t, events.RideDueEvent{
		RideRequestID: ridePollRide,
		VehicleID:     ridePollVehicle,
		ScheduledFor:  sched,
		DueAt:         time.Now(),
	})
	h.awaitActive(t, 1)
	if !h.awaitPoll(t) {
		t.Fatal("no read after ride.due; a dispatched reservation is a live ride")
	}
}

// TestRidePoller_StopsOnEveryTerminalStatus — a poller must not survive its
// ride, whichever way the ride ends.
func TestRidePoller_StopsOnEveryTerminalStatus(t *testing.T) {
	for _, status := range []string{
		ridePollStatusCompleted,
		ridePollStatusCancelled,
		ridePollStatusDeclined,
	} {
		t.Run(status, func(t *testing.T) {
			h := newRidePollHarness(t, RidePositionPollConfig{Interval: time.Hour})

			h.publish(t, acceptedEvt(nil))
			h.awaitActive(t, 1)

			h.publish(t, statusEvt(status))
			h.awaitActive(t, 0)
		})
	}
}

// TestRidePoller_AdoptsArrivedAndEnroute covers the two mid-ride transitions
// that are the only opening signal for a ride this process never saw accepted.
func TestRidePoller_AdoptsArrivedAndEnroute(t *testing.T) {
	for _, status := range []string{ridePollStatusArrived, ridePollStatusEnroute} {
		t.Run(status, func(t *testing.T) {
			h := newRidePollHarness(t, RidePositionPollConfig{Interval: time.Hour})
			h.publish(t, statusEvt(status))
			h.awaitActive(t, 1)
		})
	}
}

// TestRidePoller_TerminalForAnotherRideIsIgnored closes the orphan window the
// accept guard opens: the instant ride A goes terminal, ride B may be accepted
// on the same car. A late or duplicated terminal event for A must not tear down
// B's poller.
func TestRidePoller_TerminalForAnotherRideIsIgnored(t *testing.T) {
	h := newRidePollHarness(t, RidePositionPollConfig{Interval: time.Hour})

	h.publish(t, acceptedEvt(nil))
	h.awaitActive(t, 1)

	stale := statusEvt(ridePollStatusCompleted)
	stale.RideRequestID = "ride-previous"
	h.publish(t, stale)

	time.Sleep(50 * time.Millisecond)
	if got, ok := h.poller.PollingRide(ridePollVehicle); !ok || got != ridePollRide {
		t.Fatalf("PollingRide = (%q, %v) after a stale terminal event; want the live ride still polled", got, ok)
	}
}

// TestRidePoller_YieldsToFreshStream is the MYR-300 interaction from this side:
// while the car is streaming, the poll must not even ask Tesla. The other side
// of the gate (fields dropped if it did ask) is covered by
// TestRidePollFrame_GatedByStreamRecency in the vehicle_data tests.
func TestRidePoller_YieldsToFreshStream(t *testing.T) {
	h := newRidePollHarness(t, RidePositionPollConfig{Interval: 5 * time.Millisecond})
	h.streams.setFresh(true)

	h.publish(t, acceptedEvt(nil))
	h.awaitActive(t, 1)

	// Several intervals' worth of wall clock. Nothing should be read.
	time.Sleep(150 * time.Millisecond)
	if n := h.backfill.callCount(); n != 0 {
		t.Fatalf("vehicle_data called %d times while the car was streaming; want 0", n)
	}

	// The car goes quiet — now the poll is the only source of position.
	h.streams.setFresh(false)
	if !h.awaitPoll(t) {
		t.Fatal("no read after the stream went stale")
	}
}

// TestRidePoller_AsleepVehicleIsSkippedWithoutWaking is the never-wake
// guarantee. Tesla answers 408/503; the poller must absorb it, keep its
// schedule, and — provably — have no way to wake the car, since fakeBackfill is
// the only Tesla seam it holds.
func TestRidePoller_AsleepVehicleIsSkippedWithoutWaking(t *testing.T) {
	for name, status := range map[string]int{"408 asleep": 408, "503 unavailable": 503} {
		t.Run(name, func(t *testing.T) {
			h := newRidePollHarness(t, RidePositionPollConfig{Interval: 5 * time.Millisecond})
			h.backfill.setErr(fmt.Errorf("%w: %w",
				ErrVehicleDataRead, &FleetAPIError{StatusCode: status, Body: "vehicle unavailable"}))

			h.publish(t, acceptedEvt(nil))
			h.awaitActive(t, 1)

			// The schedule must survive the failure: a car that is asleep now
			// may be awake in 25 seconds because its driver got in.
			for i := 0; i < 3; i++ {
				if !h.awaitPoll(t) {
					t.Fatalf("polling stopped after %d asleep responses; it must keep its schedule", i)
				}
			}
			if h.poller.ActiveCount() != 1 {
				t.Fatal("poller retired itself on an asleep vehicle")
			}
		})
	}
}

// TestIsVehicleUnreachable pins the classification that keeps an asleep car out
// of the warning logs — and, just as importantly, keeps a real failure in them.
func TestIsVehicleUnreachable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"408 asleep", &FleetAPIError{StatusCode: 408}, true},
		{"503 unavailable", &FleetAPIError{StatusCode: 503}, true},
		{"wrapped 408", fmt.Errorf("read: %w", &FleetAPIError{StatusCode: 408}), true},
		{"401 revoked token", &FleetAPIError{StatusCode: 401}, false},
		{"500 tesla broken", &FleetAPIError{StatusCode: 500}, false},
		{"not a fleet error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isVehicleUnreachable(tt.err); got != tt.want {
				t.Fatalf("isVehicleUnreachable = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestRidePoller_OnePollerPerVehicle — two rides on one car must not double the
// Fleet API request rate.
func TestRidePoller_OnePollerPerVehicle(t *testing.T) {
	h := newRidePollHarness(t, RidePositionPollConfig{Interval: time.Hour})

	h.publish(t, acceptedEvt(nil))
	h.awaitActive(t, 1)

	second := acceptedEvt(nil)
	second.RideRequestID = "ride-2"
	h.publish(t, second)

	time.Sleep(50 * time.Millisecond)
	if n := h.poller.ActiveCount(); n != 1 {
		t.Fatalf("active pollers = %d, want 1 — the registry is keyed by vehicle", n)
	}
}

// TestRidePoller_DisabledStartsNothing — the kill-switch must be trustworthy.
func TestRidePoller_DisabledStartsNothing(t *testing.T) {
	bus := events.NewChannelBus(events.BusConfig{BufferSize: 8}, events.NoopBusMetrics{}, discardLogger())
	backfill := &fakeBackfill{}
	p := NewRidePositionPoller(
		bus,
		NewRidePositionPollerDeps(
			backfill, &fakeStreamRecency{},
			&fakeRideVehicles{vins: map[string]string{ridePollVehicle: ridePollVIN}},
			&stubVehicleOwner{ownerID: "user-1"},
			&fakeTokenResolver{tok: TeslaToken{AccessToken: "tok"}},
			&fakeRideLister{targets: []ActiveRideTarget{
				{RideRequestID: ridePollRide, VehicleID: ridePollVehicle, VIN: ridePollVIN},
			}},
		),
		RidePositionPollConfig{Enabled: false},
		discardLogger(),
	)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer p.Stop()

	if _, _, err := p.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	_ = bus.Publish(context.Background(), events.NewEvent(acceptedEvt(nil)))
	time.Sleep(50 * time.Millisecond)

	if n := p.ActiveCount(); n != 0 {
		t.Fatalf("active pollers = %d with the kill-switch off, want 0", n)
	}
	if n := backfill.callCount(); n != 0 {
		t.Fatalf("vehicle_data called %d times with the kill-switch off, want 0", n)
	}
}

// TestRidePoller_NoGoroutineLeakOnShutdown — Stop must be a real join, not a
// signal. A poller that survives its server is the same class of bug as one
// that survives its ride.
func TestRidePoller_NoGoroutineLeakOnShutdown(t *testing.T) {
	settle := func() int {
		for i := 0; i < 50; i++ {
			runtime.Gosched()
			time.Sleep(2 * time.Millisecond)
		}
		return runtime.NumGoroutine()
	}
	before := settle()

	bus := events.NewChannelBus(events.BusConfig{BufferSize: 16}, events.NoopBusMetrics{}, discardLogger())
	backfill := &fakeBackfill{gate: make(chan struct{}, 64)}
	p := NewRidePositionPoller(
		bus,
		NewRidePositionPollerDeps(
			backfill, &fakeStreamRecency{},
			&fakeRideVehicles{vins: map[string]string{
				ridePollVehicle: ridePollVIN, "veh-2": "7SAYGDET7TA613796",
			}},
			&stubVehicleOwner{ownerID: "user-1"},
			&fakeTokenResolver{tok: TeslaToken{AccessToken: "tok"}},
			&fakeRideLister{},
		),
		RidePositionPollConfig{Enabled: true, Interval: 5 * time.Millisecond},
		discardLogger(),
	)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	for _, vehicleID := range []string{ridePollVehicle, "veh-2"} {
		evt := acceptedEvt(nil)
		evt.VehicleID = vehicleID
		evt.RideRequestID = "ride-" + vehicleID
		_ = bus.Publish(context.Background(), events.NewEvent(evt))
	}
	deadline := time.Now().Add(2 * time.Second)
	for p.ActiveCount() != 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if p.ActiveCount() != 2 {
		t.Fatalf("active pollers = %d, want 2", p.ActiveCount())
	}
	<-backfill.gate // at least one cycle is genuinely in flight

	p.Stop()
	if n := p.ActiveCount(); n != 0 {
		t.Fatalf("registry holds %d pollers after Stop, want 0", n)
	}
	_ = bus.Close(context.Background())

	after := settle()
	// Two pollers plus the bus's per-subscription delivery goroutines is the
	// budget; anything beyond a small slack means a loop outlived Stop.
	if after > before+2 {
		t.Fatalf("goroutines: before=%d after=%d — Stop did not join its pollers", before, after)
	}
}
