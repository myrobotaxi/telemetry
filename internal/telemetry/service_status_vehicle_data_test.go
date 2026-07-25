package telemetry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// newBusMonitor builds a monitor wired to a real ChannelBus so the vehicle_data
// backfill's published VehicleTelemetryEvent can be observed (MYR-260).
func newBusMonitor(
	bus events.Bus,
	reader FleetVehicleReader,
	opts ...ServiceStatusMonitorOption,
) *ServiceStatusMonitor {
	return NewServiceStatusMonitor(
		bus,
		reader,
		&fakeTokenResolver{tok: TeslaToken{AccessToken: "tok", ExpiresAt: time.Now().Add(time.Hour)}},
		&stubVehicleOwner{ownerID: "user-1"},
		&fakeStatusUpdater{},
		nil,
		opts...,
	)
}

func vehicleDataFixture() *VehicleData {
	locked := true
	rt := 3
	odo := 42.0
	climateOn := true
	inside := 20.0
	charging := "Charging"
	port := true
	battery := 80
	return &VehicleData{
		VehicleState: &VehicleDataVehicleState{Locked: &locked, RearTrunk: &rt, Odometer: &odo},
		ClimateState: &VehicleDataClimateState{IsClimateOn: &climateOn, InsideTemp: &inside},
		ChargeState:  &VehicleDataChargeState{ChargingState: &charging, ChargePortDoorOpen: &port, BatteryLevel: &battery},
	}
}

// TestServiceStatusMonitor_VehicleDataBackfillOnInService is the core MYR-260
// path: a non-streaming (in_service) car's connectivity edge fires ONE
// vehicle_data read and republishes the mapped control fields onto the bus so
// the broadcast/persist path carries honest values instead of "— Syncing".
func TestServiceStatusMonitor_VehicleDataBackfillOnInService(t *testing.T) {
	bus := events.NewChannelBus(events.BusConfig{BufferSize: 8}, events.NoopBusMetrics{}, discardLogger())
	reader := &fakeVehicleReader{
		state: FleetVehicleState{State: "online", InService: true},
		data:  vehicleDataFixture(),
	}

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

	m := newBusMonitor(bus, reader)
	m.handleConnectivity(context.Background(), connectEvt())

	if c := reader.dataCallCount(); c != 1 {
		t.Fatalf("vehicle_data calls = %d, want 1", c)
	}

	select {
	case te := <-got:
		if te.VIN != svcTestVIN {
			t.Errorf("event VIN = %q, want %q", te.VIN, svcTestVIN)
		}
		if v, ok := te.Fields[string(FieldLocked)]; !ok || v.BoolVal == nil || !*v.BoolVal {
			t.Errorf("locked not in published event: %+v", te.Fields)
		}
		if _, ok := te.Fields[string(FieldDoorState)]; !ok {
			t.Errorf("doorState not in published event")
		}
		if _, ok := te.Fields[string(FieldChargeState)]; !ok {
			t.Errorf("chargeState not in published event")
		}
		if te.CreatedAt.IsZero() {
			t.Errorf("CreatedAt not set (needed for wire lastUpdated)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no vehicle_update event published from vehicle_data backfill")
	}
}

// TestServiceStatusMonitor_VehicleDataNoWakeOnAsleep asserts an asleep/offline
// vehicle_data error is a non-fatal skip: no publish, no panic, and the status
// read still persisted. We NEVER retry-wake.
func TestServiceStatusMonitor_VehicleDataNoWakeOnAsleep(t *testing.T) {
	bus := events.NewChannelBus(events.BusConfig{BufferSize: 8}, events.NoopBusMetrics{}, discardLogger())
	reader := &fakeVehicleReader{
		state:   FleetVehicleState{State: "offline", InService: false},
		dataErr: &FleetAPIError{StatusCode: 408, Body: "vehicle unavailable"},
	}

	got := make(chan events.VehicleTelemetryEvent, 4)
	sub, _ := bus.Subscribe(events.TopicVehicleTelemetry, func(e events.Event) {
		if te, ok := e.Payload.(events.VehicleTelemetryEvent); ok {
			got <- te
		}
	})
	defer func() { _ = bus.Unsubscribe(sub) }()

	m := newBusMonitor(bus, reader)
	m.handleConnectivity(context.Background(), connectEvt()) // offline => notStreaming => attempt

	if c := reader.dataCallCount(); c != 1 {
		t.Fatalf("vehicle_data calls = %d, want 1 (single read, no wake)", c)
	}
	select {
	case te := <-got:
		t.Fatalf("unexpected publish on asleep skip: %+v", te.Fields)
	case <-time.After(200 * time.Millisecond):
		// expected: nothing published
	}
}

// TestServiceStatusMonitor_StreamingCarSkipsVehicleData asserts an online,
// not-in-service car (actively streaming) does NOT trigger a vehicle_data read
// — the live stream already carries honest values, so no extra Fleet API load.
func TestServiceStatusMonitor_StreamingCarSkipsVehicleData(t *testing.T) {
	bus := events.NewChannelBus(events.BusConfig{BufferSize: 8}, events.NoopBusMetrics{}, discardLogger())
	reader := &fakeVehicleReader{
		state: FleetVehicleState{State: "online", InService: false},
		data:  vehicleDataFixture(),
	}
	m := newBusMonitor(bus, reader)
	m.handleConnectivity(context.Background(), connectEvt())

	if c := reader.dataCallCount(); c != 0 {
		t.Fatalf("vehicle_data calls = %d, want 0 (streaming car)", c)
	}
}

// TestServiceStatusMonitor_VehicleDataDebounced asserts the vehicle_data read
// shares the connectivity-edge per-VIN debounce: a rapid second edge within the
// cooldown does not fire a second /vehicle_data call.
func TestServiceStatusMonitor_VehicleDataDebounced(t *testing.T) {
	bus := events.NewChannelBus(events.BusConfig{BufferSize: 8}, events.NoopBusMetrics{}, discardLogger())
	reader := &fakeVehicleReader{
		state: FleetVehicleState{State: "online", InService: true},
		data:  vehicleDataFixture(),
	}

	base := time.Now()
	clock := base
	m := newBusMonitor(bus, reader,
		WithServiceReadCooldown(45*time.Second),
		withServiceClock(func() time.Time { return clock }),
	)

	m.handleConnectivity(context.Background(), connectEvt())
	clock = base.Add(5 * time.Second)
	m.handleConnectivity(context.Background(), disconnectEvt()) // within cooldown

	if c := reader.dataCallCount(); c != 1 {
		t.Fatalf("vehicle_data calls = %d, want 1 (debounced)", c)
	}

	clock = base.Add(46 * time.Second)
	m.handleConnectivity(context.Background(), connectEvt()) // cooldown elapsed
	if c := reader.dataCallCount(); c != 2 {
		t.Fatalf("vehicle_data calls after cooldown = %d, want 2", c)
	}
}

// TestServiceStatusMonitor_VehicleDataReadFailureStillSkips asserts that when
// the primary GetVehicle read fails, no vehicle_data read is attempted (we
// never had a state to decide non-streaming, and never resolved a token twice).
func TestServiceStatusMonitor_VehicleDataReadFailureStillSkips(t *testing.T) {
	bus := events.NewChannelBus(events.BusConfig{BufferSize: 8}, events.NoopBusMetrics{}, discardLogger())
	reader := &fakeVehicleReader{err: errors.New("tesla 500"), data: vehicleDataFixture()}
	m := newBusMonitor(bus, reader)
	m.handleConnectivity(context.Background(), connectEvt())

	if c := reader.dataCallCount(); c != 0 {
		t.Fatalf("vehicle_data calls = %d, want 0 (primary read failed)", c)
	}
}
