package telemetry

// MYR-394 — position on the REAL backfill path.
//
// The poller's own tests use a fake backfill, so these exercise the thing that
// actually publishes: RefreshFromVehicleData with a drive_state payload. What
// is verified here rather than assumed is the MYR-300 interaction in BOTH
// directions — that a poll's position reaches the bus when the car is quiet,
// that it is dropped when the car is streaming, and that publishing it can
// never make the car LOOK like it is streaming.

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// parkedAtServiceCentreData is the MYR-394 defect input: the car is parked
// somewhere the rider is not expecting, it is not streaming, and Tesla's REST
// object is the only thing that knows where it is. Speed is null, as Tesla
// sends for a stationary car.
func parkedAtServiceCentreData() *VehicleData {
	lat, lng := 37.7955, -122.3937
	heading := 214
	locked := true
	return &VehicleData{
		VehicleState: &VehicleDataVehicleState{Locked: &locked},
		DriveState: &VehicleDataDriveState{
			Latitude:  &lat,
			Longitude: &lng,
			Heading:   &heading,
		},
	}
}

// TestVehicleDataToFields_DriveState pins the mapping, including the two
// choices that are easy to get silently wrong.
func TestVehicleDataToFields_DriveState(t *testing.T) {
	speed := 31.5
	lat, lng := 37.7749, -122.4194
	heading := 90

	tests := []struct {
		name  string
		ds    *VehicleDataDriveState
		check func(t *testing.T, fields map[string]events.TelemetryValue)
	}{
		{
			name: "full fix",
			ds:   &VehicleDataDriveState{Latitude: &lat, Longitude: &lng, Speed: &speed, Heading: &heading},
			check: func(t *testing.T, f map[string]events.TelemetryValue) {
				loc := f[string(FieldLocation)].LocationVal
				if loc == nil || loc.Latitude != lat || loc.Longitude != lng {
					t.Fatalf("location = %+v, want (%v, %v)", loc, lat, lng)
				}
				// Heading MUST be a FloatVal: the store applies it through
				// applyFloatAsInt, which reads FloatVal, so an IntVal here
				// would be dropped without a word.
				if h := f[string(FieldHeading)].FloatVal; h == nil || *h != 90 {
					t.Fatalf("heading FloatVal = %v, want 90", h)
				}
				if f[string(FieldHeading)].IntVal != nil {
					t.Fatal("heading emitted as IntVal; the store reads FloatVal and would drop it")
				}
				if s := f[string(FieldSpeed)].FloatVal; s == nil || *s != speed {
					t.Fatalf("speed = %v, want %v", s, speed)
				}
			},
		},
		{
			name: "parked: null speed is omitted, not zeroed",
			ds:   &VehicleDataDriveState{Latitude: &lat, Longitude: &lng},
			check: func(t *testing.T, f map[string]events.TelemetryValue) {
				if _, ok := f[string(FieldSpeed)]; ok {
					t.Fatal("speed present for a stationary car; a fabricated 0 reads as a live measurement")
				}
				if f[string(FieldLocation)].LocationVal == nil {
					t.Fatal("a parked car must still contribute its position")
				}
			},
		},
		{
			name: "half a fix is not a fix",
			ds:   &VehicleDataDriveState{Latitude: &lat},
			check: func(t *testing.T, f map[string]events.TelemetryValue) {
				if _, ok := f[string(FieldLocation)]; ok {
					t.Fatal("location emitted from latitude alone")
				}
			},
		},
		{
			name:  "absent drive_state",
			ds:    nil,
			check: func(t *testing.T, f map[string]events.TelemetryValue) { t.Helper() },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fields := vehicleDataToFields(&VehicleData{DriveState: tt.ds})
			tt.check(t, fields)
		})
	}
}

// TestRidePollFrame_ReachesBusWhenNotStreaming is the fix, end to end on the
// publish path: a quiet car's REST position becomes a SourceRESTBackfill frame
// carrying `location`, which is what the broadcaster splits into latitude /
// longitude and the writer persists.
func TestRidePollFrame_ReachesBusWhenNotStreaming(t *testing.T) {
	bus := events.NewChannelBus(events.BusConfig{BufferSize: 8}, events.NoopBusMetrics{}, discardLogger())
	got := collectTelemetry(t, bus)
	reader := &fakeVehicleReader{
		state: FleetVehicleState{State: "asleep"},
		data:  parkedAtServiceCentreData(),
	}
	m := newBusMonitor(bus, reader)

	if err := m.RefreshFromVehicleData(context.Background(), svcTestVIN, "tok"); err != nil {
		t.Fatalf("RefreshFromVehicleData: %v", err)
	}

	frame, ok := awaitBackfill(t, got)
	if !ok {
		t.Fatal("no backfill frame published")
	}
	if frame.Source != events.SourceRESTBackfill {
		t.Fatalf("Source = %q, want %q", frame.Source, events.SourceRESTBackfill)
	}
	if frame.Streamed() {
		t.Fatal("poll frame reports Streamed() true; downstream would treat it as live")
	}
	loc := frame.Fields[string(FieldLocation)].LocationVal
	if loc == nil {
		t.Fatal("poll frame carries no location — the whole point of MYR-394")
	}
	if loc.Latitude != 37.7955 || loc.Longitude != -122.3937 {
		t.Fatalf("location = %+v, want the REST fix", loc)
	}
	if h := frame.Fields[string(FieldHeading)].FloatVal; h == nil || *h != 214 {
		t.Fatalf("heading = %v, want 214", h)
	}
}

// TestRidePollFrame_GatedByStreamRecency is the constraint that makes this
// feature safe to ship: a poll must NEVER out-rank fresher streamed data.
//
// Verified, not assumed. `location`, `speed` and `heading` are all in fieldMap,
// so they are in streamSourcedFields and the MYR-300 gate deletes them while
// the car is streaming. The read still fires — the gate filters fields, it does
// not suppress the call — so the assertion is on what reaches the BUS.
func TestRidePollFrame_GatedByStreamRecency(t *testing.T) {
	const window = 120 * time.Second

	tests := []struct {
		name        string
		streamAge   time.Duration
		streamed    bool
		wantGated   bool
		wantAnyPubl bool
	}{
		{name: "streaming now: position dropped", streamAge: 0, streamed: true, wantGated: true},
		{name: "inside window: position dropped", streamAge: 119 * time.Second, streamed: true, wantGated: true},
		{name: "boundary is stale: position applies", streamAge: window, streamed: true, wantAnyPubl: true},
		{name: "never streamed: position applies", wantAnyPubl: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bus := events.NewChannelBus(events.BusConfig{BufferSize: 8}, events.NoopBusMetrics{}, discardLogger())
			got := collectTelemetry(t, bus)
			reader := &fakeVehicleReader{
				state: FleetVehicleState{State: "asleep"},
				data:  parkedAtServiceCentreData(),
			}

			base := time.Now()
			clock := base
			m := newBusMonitor(bus, reader,
				WithStreamFreshness(window),
				withServiceClock(func() time.Time { return clock }),
			)

			if tt.streamed {
				m.noteStreamFrame(events.VehicleTelemetryEvent{VIN: svcTestVIN})
				clock = base.Add(tt.streamAge)
			}

			if err := m.RefreshFromVehicleData(context.Background(), svcTestVIN, "tok"); err != nil {
				t.Fatalf("RefreshFromVehicleData: %v", err)
			}

			// The REST call fires either way — the gate filters fields, it does
			// not suppress the read.
			if n := reader.dataCallCount(); n != 1 {
				t.Fatalf("vehicle_data calls = %d, want 1", n)
			}

			frame, published := awaitBackfill(t, got)
			if tt.wantGated {
				if published {
					if _, leaked := frame.Fields[string(FieldLocation)]; leaked {
						t.Fatal("position reached the bus while the car was streaming — a cached fix would displace a live one")
					}
					for _, f := range []FieldName{FieldSpeed, FieldHeading} {
						if _, leaked := frame.Fields[string(f)]; leaked {
							t.Fatalf("%s reached the bus while the car was streaming", f)
						}
					}
				}
				return
			}
			if !published {
				t.Fatal("no frame published for a non-streaming car")
			}
			if frame.Fields[string(FieldLocation)].LocationVal == nil {
				t.Fatal("position missing for a non-streaming car")
			}
		})
	}
}

// TestRidePollFrame_DoesNotLatchTheFreshnessClock is the self-latch guard from
// the position angle. If a poll's own frame stamped the recency clock, the
// FIRST poll would make the car look like it was streaming and every subsequent
// poll would gate itself out — the feature would work exactly once per ride.
func TestRidePollFrame_DoesNotLatchTheFreshnessClock(t *testing.T) {
	bus := events.NewChannelBus(events.BusConfig{BufferSize: 8}, events.NoopBusMetrics{}, discardLogger())
	got := collectTelemetry(t, bus)
	reader := &fakeVehicleReader{
		state: FleetVehicleState{State: "asleep"},
		data:  parkedAtServiceCentreData(),
	}
	m := newBusMonitor(bus, reader)

	for cycle := 1; cycle <= 3; cycle++ {
		if err := m.RefreshFromVehicleData(context.Background(), svcTestVIN, "tok"); err != nil {
			t.Fatalf("cycle %d: RefreshFromVehicleData: %v", cycle, err)
		}
		frame, ok := awaitBackfill(t, got)
		if !ok {
			t.Fatalf("cycle %d: no frame — the poll latched itself out", cycle)
		}
		if frame.Fields[string(FieldLocation)].LocationVal == nil {
			t.Fatalf("cycle %d: position gated by the previous poll's own frame", cycle)
		}
		if m.streamFresh(svcTestVIN) {
			t.Fatalf("cycle %d: a backfill frame stamped the stream-recency clock", cycle)
		}
	}
}
