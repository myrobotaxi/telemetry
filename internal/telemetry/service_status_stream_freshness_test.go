package telemetry

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// staleClimateOffData models the MYR-300 defect input: Tesla's /vehicle_data
// answers from a cache that still says climate is OFF (and the car locked)
// while the car is actually cooling and streaming HvacPower "On" to us. It
// also carries `trim`, the one REST-ONLY field, which must survive the gate.
func staleClimateOffData() *VehicleData {
	locked := true
	climateOn := false
	inside := 20.0
	port := false
	trim := "Performance"
	return &VehicleData{
		VehicleState:  &VehicleDataVehicleState{Locked: &locked},
		ClimateState:  &VehicleDataClimateState{IsClimateOn: &climateOn, InsideTemp: &inside},
		ChargeState:   &VehicleDataChargeState{ChargePortDoorOpen: &port},
		VehicleConfig: &VehicleDataVehicleConfig{TrimBadging: &trim},
	}
}

// streamFrame builds a live streamed telemetry frame for a VIN carrying
// hvacPower "On" — the state the car really is in while Tesla's cached
// /vehicle_data still reports Off. Source is left at its zero value,
// events.SourceStream, which is what makes it count toward the gate.
func streamFrame(vin string) events.VehicleTelemetryEvent {
	p := "On"
	return events.VehicleTelemetryEvent{
		VIN:       vin,
		CreatedAt: time.Now(),
		Fields:    map[string]events.TelemetryValue{string(FieldHvacPower): {StringVal: &p}},
	}
}

// collectTelemetry subscribes to the telemetry topic and returns a channel of
// published frames plus a cleanup func.
func collectTelemetry(t *testing.T, bus events.Bus) chan events.VehicleTelemetryEvent {
	t.Helper()
	got := make(chan events.VehicleTelemetryEvent, 8)
	sub, err := bus.Subscribe(events.TopicVehicleTelemetry, func(e events.Event) {
		if te, ok := e.Payload.(events.VehicleTelemetryEvent); ok && te.Source == events.SourceRESTBackfill {
			got <- te
		}
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = bus.Unsubscribe(sub) })
	return got
}

// awaitBackfill waits for the backfill's synthetic frame, or returns ok=false
// if none is published within the timeout.
func awaitBackfill(t *testing.T, got chan events.VehicleTelemetryEvent) (events.VehicleTelemetryEvent, bool) {
	t.Helper()
	select {
	case te := <-got:
		return te, true
	case <-time.After(2 * time.Second):
		return events.VehicleTelemetryEvent{}, false
	}
}

// TestServiceStatusMonitor_BackfillGatedByStreamRecency is the core MYR-300
// gate: a REST /vehicle_data backfill must NOT write stream-sourced control
// fields while the server is still receiving live frames for that VIN, because
// Tesla's cached snapshot would durably overwrite fresher streamed state
// through the COALESCE upsert. Once the stream has been quiet for a full
// freshness window the backfill applies exactly as it did before (MYR-260
// behavior for genuinely non-streaming cars is preserved).
//
// The boundary is exclusive: age == window counts as STALE.
func TestServiceStatusMonitor_BackfillGatedByStreamRecency(t *testing.T) {
	const window = 120 * time.Second

	tests := []struct {
		name         string
		streamAge    time.Duration // age of the last live frame at backfill time
		everStreamed bool
		wantGated    bool
	}{
		{name: "frame just arrived", streamAge: 0, everStreamed: true, wantGated: true},
		{name: "inside window", streamAge: 119 * time.Second, everStreamed: true, wantGated: true},
		{name: "boundary is stale", streamAge: window, everStreamed: true, wantGated: false},
		{name: "past window", streamAge: 5 * time.Minute, everStreamed: true, wantGated: false},
		{name: "never streamed", everStreamed: false, wantGated: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bus := events.NewChannelBus(events.BusConfig{BufferSize: 8}, events.NoopBusMetrics{}, discardLogger())
			got := collectTelemetry(t, bus)
			reader := &fakeVehicleReader{
				// Tesla's LAGGING connectivity view: says asleep even though
				// the car is streaming. This is what makes the backfill fire.
				state: FleetVehicleState{State: "asleep", InService: false},
				data:  staleClimateOffData(),
			}

			base := time.Now()
			clock := base
			m := newBusMonitor(bus, reader,
				WithStreamFreshness(window),
				withServiceClock(func() time.Time { return clock }),
			)

			if tt.everStreamed {
				m.handleTelemetry(context.Background(), streamFrame(svcTestVIN))
				clock = base.Add(tt.streamAge)
			}

			m.handleConnectivity(context.Background(), connectEvt())

			// The REST read itself always happens — the gate filters what is
			// written, it does not suppress the status resolution.
			if c := reader.dataCallCount(); c != 1 {
				t.Fatalf("vehicle_data calls = %d, want 1", c)
			}

			te, ok := awaitBackfill(t, got)
			if !ok {
				t.Fatal("no backfill frame published")
			}
			_, hasHvac := te.Fields[string(FieldHvacPower)]
			if tt.wantGated && hasHvac {
				t.Errorf("gated backfill still carries hvacPower: %+v", te.Fields)
			}
			if !tt.wantGated && !hasHvac {
				t.Errorf("ungated backfill dropped hvacPower: %+v", te.Fields)
			}
		})
	}
}

// TestServiceStatusMonitor_GateCoversAllControlFields asserts the rule is
// field-set-wide, not hvacPower-only: EVERY stream-sourceable field the
// backfill writes (lock, doors, climate, temps, charge-port) is dropped while
// the stream is fresh, since the same stale-cache overwrite can hit any of
// them. The REST-ONLY `trim` (Tesla never streams it) still flows — gating it
// would mean a busily-streaming car could never acquire it.
func TestServiceStatusMonitor_GateCoversAllControlFields(t *testing.T) {
	bus := events.NewChannelBus(events.BusConfig{BufferSize: 8}, events.NoopBusMetrics{}, discardLogger())
	got := collectTelemetry(t, bus)
	reader := &fakeVehicleReader{
		state: FleetVehicleState{State: "offline", InService: false},
		data:  staleClimateOffData(),
	}
	m := newBusMonitor(bus, reader, WithStreamFreshness(120*time.Second))

	m.handleTelemetry(context.Background(), streamFrame(svcTestVIN))
	m.handleConnectivity(context.Background(), connectEvt())

	te, ok := awaitBackfill(t, got)
	if !ok {
		t.Fatal("no backfill frame published (REST-only trim should still publish)")
	}

	for _, gated := range []FieldName{FieldLocked, FieldHvacPower, FieldInsideTemp, FieldChargePortDoorOpen} {
		if _, present := te.Fields[string(gated)]; present {
			t.Errorf("stream-sourced field %q survived the gate: %+v", gated, te.Fields)
		}
	}
	if v, present := te.Fields[string(FieldTrim)]; !present || v.StringVal == nil || *v.StringVal != "Performance" {
		t.Errorf("REST-only trim was dropped by the gate: %+v", te.Fields)
	}
}

// TestServiceStatusMonitor_StreamFreshnessIsPerVIN asserts one car's live
// stream never suppresses another car's backfill — the stamp is keyed by VIN.
func TestServiceStatusMonitor_StreamFreshnessIsPerVIN(t *testing.T) {
	const otherVIN = "5YJ3E1EA1PF000001"

	bus := events.NewChannelBus(events.BusConfig{BufferSize: 8}, events.NoopBusMetrics{}, discardLogger())
	got := collectTelemetry(t, bus)
	reader := &fakeVehicleReader{
		state: FleetVehicleState{State: "asleep", InService: false},
		data:  staleClimateOffData(),
	}
	m := newBusMonitor(bus, reader, WithStreamFreshness(120*time.Second))

	// A DIFFERENT car is streaming.
	m.handleTelemetry(context.Background(), streamFrame(otherVIN))
	m.handleConnectivity(context.Background(), connectEvt()) // svcTestVIN edge

	te, ok := awaitBackfill(t, got)
	if !ok {
		t.Fatal("no backfill frame published")
	}
	if _, present := te.Fields[string(FieldHvacPower)]; !present {
		t.Errorf("another VIN's stream gated this VIN's backfill: %+v", te.Fields)
	}
}

// TestServiceStatusMonitor_BackfillFrameDoesNotStampFreshness is the
// self-latching guard: the backfill publishes onto the SAME topic the monitor
// subscribes to, so if its own synthetic frame counted as "streamed" the gate
// would permanently suppress backfills for a sleeping car after the first one.
func TestServiceStatusMonitor_BackfillFrameDoesNotStampFreshness(t *testing.T) {
	bus := events.NewChannelBus(events.BusConfig{BufferSize: 8}, events.NoopBusMetrics{}, discardLogger())
	reader := &fakeVehicleReader{state: FleetVehicleState{State: "asleep"}, data: staleClimateOffData()}
	m := newBusMonitor(bus, reader, WithStreamFreshness(120*time.Second))

	m.handleTelemetry(context.Background(), events.VehicleTelemetryEvent{
		VIN:       svcTestVIN,
		CreatedAt: time.Now(),
		Fields:    map[string]events.TelemetryValue{string(FieldLocked): {}},
		Source:    events.SourceRESTBackfill,
	})

	if m.streamFresh(svcTestVIN) {
		t.Error("a REST-backfill frame stamped the stream-freshness clock")
	}
}

// TestServiceStatusMonitor_DisconnectClearsStreamFreshness asserts a
// disconnect edge drops the stamp BEFORE the backfill runs. A disconnect means
// the stream is definitively gone, so the backfill that edge exists to trigger
// must not be suppressed for the remainder of the window.
func TestServiceStatusMonitor_DisconnectClearsStreamFreshness(t *testing.T) {
	bus := events.NewChannelBus(events.BusConfig{BufferSize: 8}, events.NoopBusMetrics{}, discardLogger())
	got := collectTelemetry(t, bus)
	reader := &fakeVehicleReader{
		state: FleetVehicleState{State: "offline", InService: false},
		data:  staleClimateOffData(),
	}
	m := newBusMonitor(bus, reader, WithStreamFreshness(120*time.Second))

	m.handleTelemetry(context.Background(), streamFrame(svcTestVIN))
	m.handleConnectivity(context.Background(), disconnectEvt())

	te, ok := awaitBackfill(t, got)
	if !ok {
		t.Fatal("no backfill frame published on disconnect")
	}
	if _, present := te.Fields[string(FieldHvacPower)]; !present {
		t.Errorf("disconnect did not clear the freshness stamp: %+v", te.Fields)
	}
}

// TestDropStreamSourcedFields asserts the field partition directly: everything
// the live stream can source (fieldMap) is dropped, REST-only names are kept.
func TestDropStreamSourcedFields(t *testing.T) {
	fields := vehicleDataToFields(staleClimateOffData())
	before := len(fields)

	dropped := dropStreamSourcedFields(fields)

	if dropped == 0 {
		t.Fatal("dropped 0 stream-sourced fields from a full backfill frame")
	}
	if len(fields)+dropped != before {
		t.Fatalf("field accounting: %d kept + %d dropped != %d before", len(fields), dropped, before)
	}
	for name := range fields {
		if _, streamed := streamSourcedFields[name]; streamed {
			t.Errorf("stream-sourced field %q was not dropped", name)
		}
	}
	if _, ok := fields[string(FieldTrim)]; !ok {
		t.Error("REST-only trim was dropped")
	}
}

// TestStreamSourcedFieldsMatchesFieldMap guards the derivation: the gate's set
// must stay in lockstep with the decoder's fieldMap, so a future streamed
// field is covered without anyone remembering to update a hand-written list.
func TestStreamSourcedFieldsMatchesFieldMap(t *testing.T) {
	if len(streamSourcedFields) == 0 {
		t.Fatal("streamSourcedFields is empty")
	}
	for _, name := range fieldMap {
		if _, ok := streamSourcedFields[string(name)]; !ok {
			t.Errorf("fieldMap name %q missing from streamSourcedFields", name)
		}
	}
	// FieldTrim is deliberately absent: Tesla does not stream the trim badge
	// (MYR-279), so REST is its only source and it can never be a stale
	// overwrite of streamed state.
	if _, ok := streamSourcedFields[string(FieldTrim)]; ok {
		t.Error("FieldTrim must not be treated as stream-sourced")
	}
}
