package telemetry

// MYR-394 reconcile tests.
//
// The reconcile is what makes "a poller must not survive its ride" TRUE rather
// than merely intended, so these cover both directions — adopt what the process
// missed, reap what it should no longer be doing — plus the failure mode that
// matters most: a database blip must not be read as "no rides are active".

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// TestRidePollReconcile_AdoptsOpenRides is the restart case. Every ride that
// was live when the process died is still live when it comes back, and no bus
// event will ever mention it again — so without this the rider's marker stays
// frozen for the rest of the ride.
func TestRidePollReconcile_AdoptsOpenRides(t *testing.T) {
	h := newRidePollHarness(t, RidePositionPollConfig{Interval: time.Hour})
	h.lister.set(
		ActiveRideTarget{RideRequestID: ridePollRide, VehicleID: ridePollVehicle, VIN: ridePollVIN},
		ActiveRideTarget{RideRequestID: "ride-2", VehicleID: "veh-2", VIN: "7SAYGDET7TA613796"},
	)

	adopted, reaped, err := h.poller.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if adopted != 2 || reaped != 0 {
		t.Fatalf("Reconcile = (adopted %d, reaped %d), want (2, 0)", adopted, reaped)
	}
	if n := h.poller.ActiveCount(); n != 2 {
		t.Fatalf("active pollers = %d, want 2", n)
	}
	if !h.awaitPoll(t) {
		t.Fatal("adopted ride never polled")
	}
}

// TestRidePollReconcile_ReapsOrphans is the dropped-event case, and the reason
// this is not optional: the bus is drop-OLDEST under backpressure, so a
// terminal transition CAN go missing, and the poller it was meant to stop would
// otherwise spend Fleet API budget on a finished ride until the process ends.
func TestRidePollReconcile_ReapsOrphans(t *testing.T) {
	h := newRidePollHarness(t, RidePositionPollConfig{Interval: time.Hour})

	h.publish(t, acceptedEvt(nil))
	h.awaitActive(t, 1)

	// The ride ended, and we never heard about it.
	h.lister.set()

	adopted, reaped, err := h.poller.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if adopted != 0 || reaped != 1 {
		t.Fatalf("Reconcile = (adopted %d, reaped %d), want (0, 1)", adopted, reaped)
	}
	if n := h.poller.ActiveCount(); n != 0 {
		t.Fatalf("active pollers = %d after reaping, want 0", n)
	}
}

// TestRidePollReconcile_ReplacesPollerForANewRide covers the case where the
// process missed BOTH the end of ride A and the start of ride B on the same
// car. The registry must converge in ONE pass rather than staying subtly wrong
// until the next restart.
func TestRidePollReconcile_ReplacesPollerForANewRide(t *testing.T) {
	h := newRidePollHarness(t, RidePositionPollConfig{Interval: time.Hour})

	h.publish(t, acceptedEvt(nil))
	h.awaitActive(t, 1)

	h.lister.set(ActiveRideTarget{
		RideRequestID: "ride-next", VehicleID: ridePollVehicle, VIN: ridePollVIN,
	})

	adopted, reaped, err := h.poller.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if adopted != 1 || reaped != 1 {
		t.Fatalf("Reconcile = (adopted %d, reaped %d), want (1, 1)", adopted, reaped)
	}
	got, ok := h.poller.PollingRide(ridePollVehicle)
	if !ok || got != "ride-next" {
		t.Fatalf("PollingRide = (%q, %v), want (\"ride-next\", true)", got, ok)
	}
}

// TestRidePollReconcile_ListFailureChangesNothing — a database blip must never
// be read as "no rides are active" and tear down every live poller on the box.
func TestRidePollReconcile_ListFailureChangesNothing(t *testing.T) {
	h := newRidePollHarness(t, RidePositionPollConfig{Interval: time.Hour})

	h.publish(t, acceptedEvt(nil))
	h.awaitActive(t, 1)

	h.lister.mu.Lock()
	h.lister.err = errors.New("connection refused")
	h.lister.mu.Unlock()

	adopted, reaped, err := h.poller.Reconcile(context.Background())
	if err == nil {
		t.Fatal("Reconcile returned nil error on a failed LIST")
	}
	if adopted != 0 || reaped != 0 {
		t.Fatalf("Reconcile = (adopted %d, reaped %d) on failure, want (0, 0)", adopted, reaped)
	}
	if n := h.poller.ActiveCount(); n != 1 {
		t.Fatalf("active pollers = %d after a failed LIST, want the live one untouched (1)", n)
	}
}

// TestRidePollReconcile_IsIdempotent — the safety-net loop runs this every few
// minutes, so a steady state must not churn pollers (which would restart their
// cadence and re-fire an immediate read each pass).
func TestRidePollReconcile_IsIdempotent(t *testing.T) {
	h := newRidePollHarness(t, RidePositionPollConfig{Interval: time.Hour})
	h.lister.set(ActiveRideTarget{
		RideRequestID: ridePollRide, VehicleID: ridePollVehicle, VIN: ridePollVIN,
	})

	if _, _, err := h.poller.Reconcile(context.Background()); err != nil {
		t.Fatalf("first Reconcile: %v", err)
	}
	if !h.awaitPoll(t) {
		t.Fatal("adopted ride never polled")
	}
	callsAfterFirst := h.backfill.callCount()

	adopted, reaped, err := h.poller.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if adopted != 0 || reaped != 0 {
		t.Fatalf("second Reconcile = (adopted %d, reaped %d), want (0, 0)", adopted, reaped)
	}
	if n := h.backfill.callCount(); n != callsAfterFirst {
		t.Fatalf("vehicle_data calls went %d -> %d across an idempotent reconcile", callsAfterFirst, n)
	}
}

// TestRidePollReconcile_RespectsMaxActive — the blast-radius guard. A ride
// refused by the cap is not lost; it is simply picked up once room appears.
func TestRidePollReconcile_RespectsMaxActive(t *testing.T) {
	h := newRidePollHarness(t, RidePositionPollConfig{Interval: time.Hour, MaxActive: 1})
	h.lister.set(
		ActiveRideTarget{RideRequestID: ridePollRide, VehicleID: ridePollVehicle, VIN: ridePollVIN},
		ActiveRideTarget{RideRequestID: "ride-2", VehicleID: "veh-2", VIN: "7SAYGDET7TA613796"},
	)

	adopted, _, err := h.poller.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if adopted != 1 {
		t.Fatalf("adopted = %d with MaxActive=1, want 1", adopted)
	}
	if n := h.poller.ActiveCount(); n != 1 {
		t.Fatalf("active pollers = %d, want 1", n)
	}
}

// TestRidePollReconcile_AfterStopStartsNothing — the reconcile loop and Stop
// race by construction (both run at shutdown). A pass that lands after Stop
// must not resurrect a poller into a cancelled context.
func TestRidePollReconcile_AfterStopStartsNothing(t *testing.T) {
	bus := events.NewChannelBus(events.BusConfig{BufferSize: 8}, events.NoopBusMetrics{}, discardLogger())
	lister := &fakeRideLister{targets: []ActiveRideTarget{
		{RideRequestID: ridePollRide, VehicleID: ridePollVehicle, VIN: ridePollVIN},
	}}
	p := NewRidePositionPoller(
		bus,
		NewRidePositionPollerDeps(
			&fakeBackfill{}, &fakeStreamRecency{},
			&fakeRideVehicles{vins: map[string]string{ridePollVehicle: ridePollVIN}},
			&stubVehicleOwner{ownerID: "user-1"},
			&fakeTokenResolver{tok: TeslaToken{AccessToken: "tok"}},
			lister,
		),
		RidePositionPollConfig{Enabled: true, Interval: time.Hour},
		discardLogger(),
	)
	if err := p.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	p.Stop()

	adopted, _, err := p.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if adopted != 0 || p.ActiveCount() != 0 {
		t.Fatalf("reconcile after Stop adopted %d pollers (active %d), want 0",
			adopted, p.ActiveCount())
	}
}
