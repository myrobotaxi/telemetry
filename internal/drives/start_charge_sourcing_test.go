package drives

import (
	"context"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// runStartChargeScenario drives one full idle→drive→park sequence through a
// real Detector + bus and returns the resulting DriveStats. Each SOC argument
// is optional: a nil pointer means the corresponding frame carries no charge
// atomic group, modelling the gear(1s)/charge(slow) cadence mismatch that made
// the gear-change start frame arrive without SOC. The start frame itself never
// carries SOC — that is the realistic worst case the sourcing logic guards.
func runStartChargeScenario(t *testing.T, idleSOC, midSOC, endSOC *float64) events.DriveStats {
	t.Helper()

	bus := testBus()
	defer bus.Close(context.Background())

	cfg := testConfig()
	cfg.EndDebounce = 20 * time.Millisecond
	cfg.MinDuration = 0
	cfg.MinDistanceMiles = 0

	d := NewDetector(bus, cfg, testLogger(), NoopDetectorMetrics{}, nil)
	if err := d.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = d.Stop() }()

	startedCh := subscribeTopic(t, bus, events.TopicDriveStarted)
	endedCh := subscribeTopic(t, bus, events.TopicDriveEnded)

	const vin = "VIN_MYR241"
	now := time.Now()

	// Optional pre-drive idle tick establishes the last-known charge while
	// parked (gear=P, so no drive is started).
	if idleSOC != nil {
		publishTelemetry(t, bus, telemetryEvent(vin, now, map[string]events.TelemetryValue{
			string(telemetry.FieldGear): {StringVal: ptr("P")},
			string(telemetry.FieldSOC):  {FloatVal: idleSOC},
		}))
	}

	// Drive start frame: gear flips to D but carries NO SOC.
	publishTelemetry(t, bus, telemetryEvent(vin, now.Add(1*time.Second), map[string]events.TelemetryValue{
		string(telemetry.FieldGear):     {StringVal: ptr("D")},
		string(telemetry.FieldSpeed):    {FloatVal: ptr(0.0)},
		string(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: 33.0, Longitude: -96.0}},
	}))
	if _, ok := waitForEvent(startedCh); !ok {
		t.Fatal("timed out waiting for DriveStartedEvent")
	}

	// Mid-drive frame: movement plus an optional SOC sample.
	midFields := map[string]events.TelemetryValue{
		string(telemetry.FieldGear):     {StringVal: ptr("D")},
		string(telemetry.FieldSpeed):    {FloatVal: ptr(55.0)},
		string(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: 33.2, Longitude: -96.2}},
	}
	if midSOC != nil {
		midFields[string(telemetry.FieldSOC)] = events.TelemetryValue{FloatVal: midSOC}
	}
	publishTelemetry(t, bus, telemetryEvent(vin, now.Add(61*time.Second), midFields))

	// Park frame ends the drive, at a distinct location so distance > 0.
	stopFields := map[string]events.TelemetryValue{
		string(telemetry.FieldGear):     {StringVal: ptr("P")},
		string(telemetry.FieldSpeed):    {FloatVal: ptr(0.0)},
		string(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: 33.3, Longitude: -96.3}},
	}
	if endSOC != nil {
		stopFields[string(telemetry.FieldSOC)] = events.TelemetryValue{FloatVal: endSOC}
	}
	publishTelemetry(t, bus, telemetryEvent(vin, now.Add(120*time.Second), stopFields))

	evt, ok := waitForEvent(endedCh)
	if !ok {
		t.Fatal("timed out waiting for DriveEndedEvent")
	}
	return evt.Payload.(events.DriveEndedEvent).Stats
}

// TestDetector_StartChargeLevelSourcing is the MYR-241 table-driven proof that
// the state machine sources the drive-start charge correctly across every SOC
// arrival ordering. The DriveEndedEvent it produces is the sole input to the
// store's completion write, so a correct Stats.StartChargeLevel here is what
// ultimately lands a non-zero startChargeLevel in the DB.
func TestDetector_StartChargeLevelSourcing(t *testing.T) {
	soc := func(v float64) *float64 { return &v }

	tests := []struct {
		name      string
		idleSOC   *float64 // SOC seen while parked, before the drive
		midSOC    *float64 // first in-drive SOC sample
		endSOC    *float64 // SOC on the parking frame
		wantStart int
		wantEnd   int
	}{
		{
			// (a) SOC known before drive start → seeded from last-known charge.
			name:      "soc known before start is recorded",
			idleSOC:   soc(80),
			wantStart: 80,
			wantEnd:   80, // lastSOC is seeded to the start charge when none arrives later
		},
		{
			// (a + ownership) A real captured start value is never overwritten
			// by a later in-drive sample (first-write-wins).
			name:      "known start charge not overwritten mid-drive",
			idleSOC:   soc(80),
			midSOC:    soc(70),
			endSOC:    soc(65),
			wantStart: 80,
			wantEnd:   65,
		},
		{
			// (b) SOC first arrives mid-drive → start patched exactly once from
			// the first in-drive sample, not the later ones.
			name:      "soc first arrives mid-drive patches start once",
			midSOC:    soc(70),
			endSOC:    soc(65),
			wantStart: 70,
			wantEnd:   65,
		},
		{
			// (c) SOC never observed anywhere → start stays 0 and end is
			// unchanged (also 0). The mapper must not invent a value.
			name:      "soc never observed stays zero",
			wantStart: 0,
			wantEnd:   0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stats := runStartChargeScenario(t, tt.idleSOC, tt.midSOC, tt.endSOC)

			if stats.StartChargeLevel != tt.wantStart {
				t.Errorf("StartChargeLevel = %d, want %d", stats.StartChargeLevel, tt.wantStart)
			}
			// (d) endChargeLevel behaviour is unchanged by the start-sourcing
			// logic across every ordering.
			if stats.EndChargeLevel != tt.wantEnd {
				t.Errorf("EndChargeLevel = %d, want %d", stats.EndChargeLevel, tt.wantEnd)
			}
		})
	}
}
