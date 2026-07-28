package telemetry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/commands"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// recordingWaker records how it was asked to wake and can script a failure.
type recordingWaker struct {
	calls int
	err   error
	// probeAwake, when non-nil, is invoked so a test can assert the refresher
	// hands over a probe that actually consults the Fleet API.
	runProbe bool
	probeSaw bool
	probeErr error
}

func (w *recordingWaker) EnsureAwake(ctx context.Context, _, _ string, probe commands.AwakeProbe) error {
	w.calls++
	if w.runProbe {
		awake, err := probe(ctx)
		w.probeSaw, w.probeErr = awake, err
	}
	return w.err
}

func newRefresherFor(
	m *ServiceStatusMonitor,
	reader vehicleStateProbe,
	waker vehicleWaker,
	now time.Time,
) *VehicleRefresher {
	return NewVehicleRefresher(
		m,
		&fakeTokenResolver{tok: TeslaToken{AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour)}},
		reader,
		waker,
		m,
		discardLogger(),
		withRefreshClock(func() time.Time { return now }),
	)
}

// A fresh stream short-circuits before the token resolver, the waker, and the
// Fleet API — the "free" path.
func TestVehicleRefresher_FreshShortCircuit(t *testing.T) {
	now := refreshFixedNow
	streamedAt := now.Add(-10 * time.Second)

	reader := &fakeVehicleReader{state: FleetVehicleState{State: "online"}}
	m := newBusMonitor(nil, reader, withServiceClock(func() time.Time { return now }))
	m.lastStream.Store(svcTestVIN, streamedAt)

	waker := &recordingWaker{}
	res, err := newRefresherFor(m, reader, waker, now).Refresh(context.Background(), "user-1", svcTestVIN)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if res.Status != RefreshStatusFresh {
		t.Fatalf("status = %q want %q", res.Status, RefreshStatusFresh)
	}
	if !res.LastUpdated.Equal(streamedAt) {
		t.Fatalf("lastUpdated = %v want %v", res.LastUpdated, streamedAt)
	}
	if waker.calls != 0 {
		t.Fatalf("wake calls = %d want 0", waker.calls)
	}
	if reader.callCount() != 0 || reader.dataCallCount() != 0 {
		t.Fatalf("Fleet API was dialed on a fresh short-circuit (get=%d data=%d)",
			reader.callCount(), reader.dataCallCount())
	}
}

// A stale stream wakes, reads once, and publishes through the MYR-260 backfill
// mapping — the assertion that the refresh endpoint reuses the existing
// pipeline rather than inventing a second one. The published frame must be
// marked SourceRESTBackfill so it can never latch the MYR-300 freshness gate.
func TestVehicleRefresher_RefreshedPublishesThroughBackfill(t *testing.T) {
	now := refreshFixedNow
	bus := events.NewChannelBus(events.BusConfig{BufferSize: 8}, events.NoopBusMetrics{}, discardLogger())

	got := make(chan events.VehicleTelemetryEvent, 4)
	sub, err := bus.Subscribe(events.TopicVehicleTelemetry, func(e events.Event) {
		if te, ok := e.Payload.(events.VehicleTelemetryEvent); ok {
			got <- te
		}
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = bus.Unsubscribe(sub) }()

	reader := &fakeVehicleReader{
		state: FleetVehicleState{State: "online"},
		data:  vehicleDataFixture(),
	}
	// No lastStream stamp at all => stale by definition, so the MYR-300 gate
	// is a no-op and the full field set flows.
	m := newBusMonitor(bus, reader, withServiceClock(func() time.Time { return now }))

	waker := &recordingWaker{runProbe: true}
	res, err := newRefresherFor(m, reader, waker, now).Refresh(context.Background(), "user-1", svcTestVIN)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if res.Status != RefreshStatusRefreshed {
		t.Fatalf("status = %q want %q", res.Status, RefreshStatusRefreshed)
	}
	if !res.LastUpdated.Equal(now) {
		t.Fatalf("lastUpdated = %v want %v", res.LastUpdated, now)
	}
	if waker.calls != 1 {
		t.Fatalf("wake calls = %d want 1", waker.calls)
	}
	if !waker.probeSaw || waker.probeErr != nil {
		t.Fatalf("probe reported awake=%v err=%v; want awake=true on an online car", waker.probeSaw, waker.probeErr)
	}
	if c := reader.dataCallCount(); c != 1 {
		t.Fatalf("vehicle_data calls = %d want exactly 1", c)
	}

	select {
	case te := <-got:
		if te.VIN != svcTestVIN {
			t.Errorf("event VIN = %q want %q", te.VIN, svcTestVIN)
		}
		if te.Source != events.SourceRESTBackfill {
			t.Errorf("event Source = %v want SourceRESTBackfill", te.Source)
		}
		if te.Streamed() {
			t.Error("a REST-sourced refresh frame must not count as streamed (MYR-300)")
		}
		if !te.CreatedAt.Equal(now) {
			t.Errorf("event CreatedAt = %v want %v", te.CreatedAt, now)
		}
		// Spot-check the MYR-260 mapping actually ran rather than an empty frame.
		if v, ok := te.Fields[string(FieldLocked)]; !ok || v.BoolVal == nil || !*v.BoolVal {
			t.Errorf("published frame missing mapped `locked` field: %+v", te.Fields)
		}
		if _, ok := te.Fields[string(FieldOdometer)]; !ok {
			t.Errorf("published frame missing mapped `odometer` field: %+v", te.Fields)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the backfill frame")
	}
}

// vehicle_config rides along on the same read, so an owner-triggered refresh
// re-acquires seatCoolingCapable (MYR-308) for free.
func TestVehicleRefresher_RefreshesSeatCoolingCapable(t *testing.T) {
	now := refreshFixedNow
	bus := events.NewChannelBus(events.BusConfig{BufferSize: 8}, events.NoopBusMetrics{}, discardLogger())

	got := make(chan events.VehicleTelemetryEvent, 4)
	sub, err := bus.Subscribe(events.TopicVehicleTelemetry, func(e events.Event) {
		if te, ok := e.Payload.(events.VehicleTelemetryEvent); ok {
			got <- te
		}
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer func() { _ = bus.Unsubscribe(sub) }()

	capable := true
	data := vehicleDataFixture()
	data.VehicleConfig = &VehicleDataVehicleConfig{HasSeatCooling: &capable}

	reader := &fakeVehicleReader{state: FleetVehicleState{State: "online"}, data: data}
	m := newBusMonitor(bus, reader, withServiceClock(func() time.Time { return now }))

	if _, err := newRefresherFor(m, reader, &recordingWaker{}, now).
		Refresh(context.Background(), "user-1", svcTestVIN); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	select {
	case te := <-got:
		v, ok := te.Fields[string(FieldSeatCoolingCapable)]
		if !ok || v.BoolVal == nil || !*v.BoolVal {
			t.Fatalf("refresh did not carry seatCoolingCapable: %+v", te.Fields)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the backfill frame")
	}
}

// A spent wake budget stops before the read: the car is asleep, so there is
// nothing to read and the typed 503 propagates untouched.
func TestVehicleRefresher_WakeFailureSkipsRead(t *testing.T) {
	now := refreshFixedNow
	reader := &fakeVehicleReader{state: FleetVehicleState{State: "asleep"}, data: vehicleDataFixture()}
	m := newBusMonitor(nil, reader, withServiceClock(func() time.Time { return now }))

	asleep := commands.NewExecutor(
		&alwaysAwakeTransport{},
		discardLogger(),
		commands.WithConfig(commands.Config{WakeMaxAttempts: 1, WakeBackoff: time.Millisecond, CounterRetryMax: 1}),
	)
	// Wire the REAL executor with the refresher's own probe so the whole
	// wake path is exercised end to end against a car that never wakes.
	r := newRefresherFor(m, reader, asleep, now)

	_, err := r.Refresh(context.Background(), "user-1", svcTestVIN)
	if err == nil {
		t.Fatal("expected a vehicle_asleep error")
	}
	var cErr *commands.CommandError
	if !errors.As(err, &cErr) {
		t.Fatalf("error is not *commands.CommandError: %v", err)
	}
	if cErr.Code != wserrors.ErrCodeVehicleAsleep {
		t.Fatalf("code = %q want %q", cErr.Code, wserrors.ErrCodeVehicleAsleep)
	}
	if c := reader.dataCallCount(); c != 0 {
		t.Fatalf("vehicle_data calls = %d want 0 — a car that never woke must not be read", c)
	}
}

// A failed vehicle_data read surfaces as the classified sentinel so the handler
// can tell "the car went back to sleep" from "the bus is broken".
func TestVehicleRefresher_ReadFailureIsClassified(t *testing.T) {
	now := refreshFixedNow
	reader := &fakeVehicleReader{
		state:   FleetVehicleState{State: "online"},
		dataErr: &FleetAPIError{StatusCode: 408, Body: "vehicle unavailable"},
	}
	m := newBusMonitor(nil, reader, withServiceClock(func() time.Time { return now }))

	_, err := newRefresherFor(m, reader, &recordingWaker{}, now).
		Refresh(context.Background(), "user-1", svcTestVIN)
	if !errors.Is(err, ErrVehicleDataRead) {
		t.Fatalf("error = %v, want it to wrap ErrVehicleDataRead", err)
	}
	var apiErr *FleetAPIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 408 {
		t.Fatalf("error did not preserve the Fleet 408: %v", err)
	}
}
