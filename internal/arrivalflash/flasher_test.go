package arrivalflash

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/commands"
	"github.com/myrobotaxi/telemetry/internal/events"
)

const (
	testVIN     = "7SAXCDE2NTF000001" // synthetic — VINs are P1 (see internal/vin)
	testRideID  = "cride_542"
	testVehicle = "cveh_1"
	testOwner   = "cusr_owner"
	testToken   = "tesla-access-token" //nolint:gosec // synthetic test value, not a credential
)

// --- doubles -----------------------------------------------------------

// recorder is the single timeline every test reads. Commands and inter-flash
// sleeps land in ONE ordered slice on purpose: the property under test is not
// "three commands were sent" but "three commands were sent with a gap between
// them", and two separate counters cannot tell those apart.
type recorder struct {
	mu       sync.Mutex
	timeline []string
	reqs     []commands.Request
	sleeps   []time.Duration
	// failAt is the 1-based flash number that fails; 0 means none fail.
	failAt int
	calls  int
}

var errCarRefused = errors.New("vehicle command transport error")

func (r *recorder) Execute(_ context.Context, req commands.Request) (commands.Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.reqs = append(r.reqs, req)
	r.timeline = append(r.timeline, "cmd:"+req.Command)
	if r.failAt == r.calls {
		return commands.Result{}, errCarRefused
	}
	return commands.Result{Command: req.Command, Applied: true}, nil
}

// sleeper is the injected pacing. It records the requested gap and returns
// immediately, so a test spends no wall clock on a 1.5 s interval.
func (r *recorder) sleeper(_ context.Context, d time.Duration) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sleeps = append(r.sleeps, d)
	r.timeline = append(r.timeline, "sleep:"+d.String())
	return nil
}

func (r *recorder) snapshot() ([]string, []commands.Request, []time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.timeline...),
		append([]commands.Request(nil), r.reqs...),
		append([]time.Duration(nil), r.sleeps...)
}

type fakeVehicles struct {
	vin string
	err error
}

func (v *fakeVehicles) ResolveVIN(context.Context, string) (string, error) {
	if v.err != nil {
		return "", v.err
	}
	return v.vin, nil
}

type fakeTokens struct {
	token string
	err   error
}

func (t *fakeTokens) ResolveToken(context.Context, string) (string, error) {
	if t.err != nil {
		return "", t.err
	}
	return t.token, nil
}

// recordingBus hands the test the handler the Flasher subscribed with, so an
// event can be delivered exactly as the bus would deliver it — serially, on the
// caller's goroutine.
type recordingBus struct {
	mu      sync.Mutex
	handler events.Handler
	topic   events.Topic
	subs    int
	unsubs  int
}

func (b *recordingBus) Publish(context.Context, events.Event) error { return nil }

func (b *recordingBus) Subscribe(topic events.Topic, h events.Handler) (events.Subscription, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handler, b.topic = h, topic
	b.subs++
	return events.Subscription{ID: "sub", Topic: topic}, nil
}

func (b *recordingBus) Unsubscribe(events.Subscription) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.unsubs++
	return nil
}

func (b *recordingBus) Close(context.Context) error { return nil }

func (b *recordingBus) deliver(t *testing.T, ev events.RideWaypointArrivedEvent) {
	t.Helper()
	b.mu.Lock()
	h := b.handler
	b.mu.Unlock()
	if h == nil {
		t.Fatal("nothing subscribed to ride.waypoint_arrived")
	}
	h(events.NewEvent(ev))
}

func (b *recordingBus) subscribed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.handler != nil
}

// --- harness -----------------------------------------------------------

type harness struct {
	flasher  *Flasher
	bus      *recordingBus
	rec      *recorder
	vehicles *fakeVehicles
	tokens   *fakeTokens
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	h := &harness{
		bus:      &recordingBus{},
		rec:      &recorder{},
		vehicles: &fakeVehicles{vin: testVIN},
		tokens:   &fakeTokens{token: testToken},
	}
	h.flasher = New(h.bus, h.vehicles, h.tokens, h.rec, cfg,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.flasher.setSleeper(h.rec.sleeper)
	if err := h.flasher.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = h.flasher.Stop() })
	return h
}

func arrivedEvent(waypoint string) events.RideWaypointArrivedEvent {
	return events.RideWaypointArrivedEvent{
		RideRequestID: testRideID,
		VehicleID:     testVehicle,
		RiderID:       "cusr_rider",
		OwnerID:       testOwner,
		Waypoint:      waypoint,
	}
}

// deliverAndSettle delivers one arrival and blocks until its greeting is done.
// Wait is the quiesce point the shutdown path uses, which is what makes this
// deterministic rather than a sleep-and-hope.
func (h *harness) deliverAndSettle(t *testing.T, ev events.RideWaypointArrivedEvent) {
	t.Helper()
	h.bus.deliver(t, ev)
	h.flasher.Wait()
}

// --- tests -------------------------------------------------------------

// TestFlasherSendsThreeSpacedFlashes is the client's decision, pinned: THREE
// flash_lights commands, one interval apart, per observed arrival — and the
// spacing is asserted as ORDER, not just as a count of sleeps, because a
// greeting that fires three commands back to back and then sleeps twice would
// pass a count-only test while looking, from the pavement, like one flash.
func TestFlasherSendsThreeSpacedFlashes(t *testing.T) {
	h := newHarness(t, DefaultConfig())

	h.deliverAndSettle(t, arrivedEvent(events.WaypointPickup))

	timeline, reqs, sleeps := h.rec.snapshot()
	want := []string{
		"cmd:flash_lights",
		"sleep:1.5s",
		"cmd:flash_lights",
		"sleep:1.5s",
		"cmd:flash_lights",
	}
	if strings.Join(timeline, ",") != strings.Join(want, ",") {
		t.Errorf("greeting timeline = %v, want %v", timeline, want)
	}
	if len(reqs) != defaultFlashes {
		t.Fatalf("sent %d commands, want %d", len(reqs), defaultFlashes)
	}
	for i, gap := range sleeps {
		if gap != defaultInterval {
			t.Errorf("gap %d = %v, want %v", i+1, gap, defaultInterval)
		}
	}
}

// TestFlasherSendsTheSignedOwnerAuthenticatedCommand pins WHAT goes to the car:
// the registry's `flash_lights` (SignerRequired), against the resolved VIN,
// bearing the OWNER's token — the same executor + token seam the nav dispatcher
// uses, not a second path to the vehicle. It also pins the absence of params,
// since flash_lights takes none.
func TestFlasherSendsTheSignedOwnerAuthenticatedCommand(t *testing.T) {
	h := newHarness(t, DefaultConfig())

	h.deliverAndSettle(t, arrivedEvent(events.WaypointPickup))

	_, reqs, _ := h.rec.snapshot()
	if len(reqs) == 0 {
		t.Fatal("no command sent")
	}
	for i, req := range reqs {
		if req.Command != commandFlashLights {
			t.Errorf("command %d = %q, want %q", i+1, req.Command, commandFlashLights)
		}
		if req.VIN != testVIN {
			t.Errorf("command %d VIN = %q, want %q", i+1, req.VIN, testVIN)
		}
		if req.AccessToken != testToken {
			t.Errorf("command %d used the wrong token", i+1)
		}
		if len(req.Params) != 0 {
			t.Errorf("command %d carried params %v; flash_lights takes none", i+1, req.Params)
		}
	}
}

// TestFlasherIsExactlyOncePerWaypoint covers the latch. The detector is already
// exactly-once, so this is the belt-and-braces half — and it is worth having
// because the failure it prevents is one a person on the pavement can see: a
// car flashing six times looks broken, not welcoming.
//
// The second case is the reason the key is (ride, waypoint) and not the ride
// alone: a ride has more than one leg endpoint, and a drop-off arrival must
// still be able to greet after the pickup already did.
func TestFlasherIsExactlyOncePerWaypoint(t *testing.T) {
	tests := []struct {
		name      string
		waypoints []string
		want      int
	}{
		{
			name:      "redelivery of the same waypoint flashes once",
			waypoints: []string{events.WaypointPickup, events.WaypointPickup, events.WaypointPickup},
			want:      defaultFlashes,
		},
		{
			name:      "a different waypoint on the same ride greets again",
			waypoints: []string{events.WaypointPickup, "dropoff"},
			want:      2 * defaultFlashes,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, DefaultConfig())
			for _, wp := range tt.waypoints {
				h.deliverAndSettle(t, arrivedEvent(wp))
			}
			if _, reqs, _ := h.rec.snapshot(); len(reqs) != tt.want {
				t.Errorf("sent %d commands, want %d", len(reqs), tt.want)
			}
		})
	}
}

// TestFlasherAbandonsTheRemainderOnFailure pins the bounded, no-retry policy:
// the failed flash is not repeated AND the rest of the greeting is dropped, so
// a waypoint can never cost more than three commands however badly it goes.
//
// Abandoning rather than pressing on is the documented call. A refusal here —
// unpaired virtual key, expired scope, a car that would not wake inside the
// executor's own budget — is not a condition that clears in 1.5 s, and a
// partial greeting (flash, gap, nothing) reads as a fault where silence reads
// as a car that simply did not flash.
func TestFlasherAbandonsTheRemainderOnFailure(t *testing.T) {
	tests := []struct {
		name     string
		failAt   int
		wantCmds int
	}{
		{name: "first flash fails: nothing further is attempted", failAt: 1, wantCmds: 1},
		{name: "second flash fails: the third is abandoned", failAt: 2, wantCmds: 2},
		{name: "last flash fails: the greeting was over anyway", failAt: 3, wantCmds: 3},
		{name: "no failure sends the full three", failAt: 0, wantCmds: 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, DefaultConfig())
			h.rec.failAt = tt.failAt

			h.deliverAndSettle(t, arrivedEvent(events.WaypointPickup))

			_, reqs, _ := h.rec.snapshot()
			if len(reqs) != tt.wantCmds {
				t.Errorf("sent %d commands, want %d", len(reqs), tt.wantCmds)
			}
			if len(reqs) > defaultFlashes {
				t.Errorf("sent %d commands for one waypoint; the ceiling is %d", len(reqs), defaultFlashes)
			}
		})
	}
}

// TestFlasherLatchHoldsThroughAFailedGreeting is the other half of "bounded":
// a greeting that failed is still spent. A redelivery must not become a retry
// by the back door, because "at most three commands per waypoint" is a promise
// about the WAYPOINT, not about one delivery of its event.
func TestFlasherLatchHoldsThroughAFailedGreeting(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	h.rec.failAt = 1

	h.deliverAndSettle(t, arrivedEvent(events.WaypointPickup))
	h.deliverAndSettle(t, arrivedEvent(events.WaypointPickup))

	if _, reqs, _ := h.rec.snapshot(); len(reqs) != 1 {
		t.Errorf("sent %d commands, want 1 — the redelivery retried a spent greeting", len(reqs))
	}
}

// TestFlasherKillSwitchOffTouchesNothing pins ARRIVAL_FLASH_ENABLED=false as
// "the component does not exist": no subscription at all, so no event can even
// reach it and no car is ever dialled. Start still returns nil — off is not an
// error — and Stop on a never-started Flasher must not blow up either, because
// the wiring calls it unconditionally on shutdown.
func TestFlasherKillSwitchOffTouchesNothing(t *testing.T) {
	bus := &recordingBus{}
	rec := &recorder{}
	cfg := DefaultConfig()
	cfg.Enabled = false

	f := New(bus, &fakeVehicles{vin: testVIN}, &fakeTokens{token: testToken}, rec, cfg,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := f.Start(context.Background()); err != nil {
		t.Fatalf("Start with the switch off should be a silent no-op, got: %v", err)
	}
	if bus.subscribed() {
		t.Error("a disabled Flasher subscribed to ride.waypoint_arrived")
	}
	if err := f.Stop(); err != nil {
		t.Errorf("Stop on a never-started Flasher: %v", err)
	}
	if _, reqs, _ := rec.snapshot(); len(reqs) != 0 {
		t.Errorf("a disabled Flasher sent %d commands, want 0", len(reqs))
	}
}

// TestFlasherSkipsWhenTheCarOrOwnerCannotBeResolved pins the two pre-command
// failures as silent skips that dial nothing. Both are ordinary — a vehicle
// deleted between the arrival and this event, an owner who unlinked Tesla — and
// neither is worth telling anybody about, because the ride has already advanced
// and a missed courtesy is not actionable by any party.
func TestFlasherSkipsWhenTheCarOrOwnerCannotBeResolved(t *testing.T) {
	tests := []struct {
		name     string
		vinErr   error
		tokenErr error
	}{
		{name: "vehicle vanished", vinErr: fmt.Errorf("resolve vin: %w", errCarRefused)},
		{name: "owner token unavailable", tokenErr: fmt.Errorf("resolve token: %w", errCarRefused)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, DefaultConfig())
			h.vehicles.err = tt.vinErr
			h.tokens.err = tt.tokenErr

			h.deliverAndSettle(t, arrivedEvent(events.WaypointPickup))

			if _, reqs, _ := h.rec.snapshot(); len(reqs) != 0 {
				t.Errorf("sent %d commands, want 0", len(reqs))
			}
		})
	}
}

// TestFlasherIgnoresAForeignPayload pins the type-assertion guard: a wrong
// payload on the topic is logged and dropped, never a panic on the bus's
// delivery goroutine (which is shared with every other subscriber's turn).
func TestFlasherIgnoresAForeignPayload(t *testing.T) {
	h := newHarness(t, DefaultConfig())

	h.bus.handler(events.NewEvent(events.RideWaypointArrivedEvent{}))
	h.flasher.Wait()
	// A zero-valued event is still a valid payload and greets once; the point
	// of this test is the line below it, which is not.
	h.bus.handler(events.Event{ID: "x", Topic: events.TopicRideWaypointArrived, Payload: events.RideStatusChangedEvent{}})
	h.flasher.Wait()

	if _, reqs, _ := h.rec.snapshot(); len(reqs) != defaultFlashes {
		t.Errorf("sent %d commands, want %d — the foreign payload was acted on", len(reqs), defaultFlashes)
	}
}

// TestFlasherStopEndsTheGreetingInFlight pins the shutdown seam: Stop cancels
// the greeting's context, so a sequence parked between flashes gives up rather
// than holding the drain open for the rest of its interval budget.
//
// The cancellation is triggered FROM the first gap — with the production
// sleepCtx doing the parking — so what the test exercises is the real
// interaction between Stop's cancel and the pacing, not a stand-in for it.
func TestFlasherStopEndsTheGreetingInFlight(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	h.flasher.setSleeper(func(ctx context.Context, d time.Duration) error {
		_ = h.flasher.Stop() // cancels the parent of this greeting's context
		return sleepCtx(ctx, d)
	})

	settled := make(chan struct{})
	go func() {
		defer close(settled)
		h.deliverAndSettle(t, arrivedEvent(events.WaypointPickup))
	}()

	select {
	case <-settled:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not end the greeting in flight — it waited out the interval")
	}
	if _, reqs, _ := h.rec.snapshot(); len(reqs) != 1 {
		t.Errorf("sent %d commands, want 1 — the greeting should stop at the cancelled gap", len(reqs))
	}
}
