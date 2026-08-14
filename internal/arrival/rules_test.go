package arrival

import (
	"math"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// Reference pickup, and points at known distances from it. Downtown LA, the
// client's test bed. 0.001° of latitude is ~111 m, so the offsets below
// straddle the 80 m radius by a wide, unambiguous margin.
const (
	pickupLat = 34.0522
	pickupLng = -118.2437
)

// offsetNorth returns a coordinate `meters` north of the pickup.
func offsetNorth(meters float64) (float64, float64) {
	const metersPerDegreeLat = 111_320.0
	return pickupLat + meters/metersPerDegreeLat, pickupLng
}

func TestHaversineMeters(t *testing.T) {
	tests := []struct {
		name   string
		meters float64
	}{
		{"same point", 0},
		{"50 m", 50},
		{"80 m — the radius itself", 80},
		{"500 m", 500},
		{"5 km", 5000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lat, lng := offsetNorth(tt.meters)
			got := haversineMeters(pickupLat, pickupLng, lat, lng)
			// The offset helper uses a flat 111,320 m per degree of latitude,
			// which is itself an approximation — hence a tolerance that scales
			// with the distance rather than a flat metre.
			tolerance := math.Max(1, tt.meters*0.002)
			if math.Abs(got-tt.meters) > tolerance {
				t.Errorf("haversineMeters = %.2f m, want ~%.2f m (±%.2f)", got, tt.meters, tolerance)
			}
		})
	}
}

func TestFixStopped(t *testing.T) {
	mph := func(v float64) *float64 { return &v }

	tests := []struct {
		name string
		fix  Fix
		want bool
	}{
		{"zero speed is stopped", Fix{Speed: mph(0)}, true},
		{"jitter at the threshold is stopped", Fix{Speed: mph(1)}, true},
		{"walking pace is moving", Fix{Speed: mph(3)}, false},
		{"traffic speed is moving", Fix{Speed: mph(28)}, false},
		// Speed decides on its own when present: a car reporting motion is
		// moving whatever its gear says.
		{"reported motion beats gear P", Fix{Speed: mph(4), Gear: gearPark}, false},
		{"speed absent, gear P is stopped", Fix{Gear: gearPark}, true},
		{"speed absent, gear D is not evidence", Fix{Gear: "D"}, false},
		// Silence is not stillness — the whole point of the nullable Speed.
		{"speed and gear both absent is not evidence", Fix{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.fix.stopped(defaultStoppedSpeedMPH); got != tt.want {
				t.Errorf("stopped() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestTrackObserveRadiusAndDwell drives the rule with scripted frame sequences
// — the shape a real car produces — rather than asserting on one frame at a
// time. Each step is (seconds since t0, meters from pickup, mph), and the test
// asserts the step at which the rule first reports arrival.
func TestTrackObserveRadiusAndDwell(t *testing.T) {
	t0 := time.Date(2026, 8, 13, 17, 0, 0, 0, time.UTC)
	cfg := DefaultConfig() // 80 m, 20 s, 1 mph

	type step struct {
		sec    int
		meters float64
		mph    float64
	}
	tests := []struct {
		name string
		seq  []step
		// wantFireAt is the index of the first step that must report arrival,
		// or -1 when no step may.
		wantFireAt int
	}{
		{
			name: "a car that parks at the kerb fires once the dwell completes",
			seq: []step{
				{0, 400, 30}, {5, 150, 18}, {10, 30, 6}, {15, 12, 0},
				{25, 12, 0}, {34, 12, 0}, {35, 12, 0},
			},
			// Stillness starts at t=15; 20 s of it completes at t=35.
			wantFireAt: 6,
		},
		{
			name: "a moving car inside the radius never fires",
			seq: []step{
				{0, 70, 22}, {20, 40, 19}, {40, 20, 15}, {60, 10, 12}, {120, 5, 9},
			},
			wantFireAt: -1,
		},
		{
			name: "a car stopped far away never fires",
			seq: []step{
				{0, 900, 0}, {30, 900, 0}, {60, 900, 0}, {600, 900, 0},
			},
			wantFireAt: -1,
		},
		{
			name: "the radius boundary is inclusive, the far side is not",
			seq: []step{
				{0, 81, 0}, {30, 81, 0}, // outside: never accrues
				{60, 80, 0}, {80, 80, 0}, // inside from t=60; 20 s completes at t=80
			},
			wantFireAt: 3,
		},
		{
			name: "an interrupted dwell resets — the clock restarts, it does not resume",
			seq: []step{
				{0, 10, 0}, {10, 10, 0}, {15, 10, 0}, // 15 s accrued...
				{18, 10, 14},             // ...and thrown away: the car moved off
				{20, 10, 0}, {30, 10, 0}, // restart at t=20
				{39, 10, 0}, // only 19 s in — still short
				{40, 10, 0}, // 20 s from t=20
			},
			wantFireAt: 7,
		},
		{
			name: "leaving the radius mid-dwell resets it too",
			seq: []step{
				{0, 10, 0}, {10, 10, 0},
				{12, 200, 0}, // stopped, but at the lights up the road
				{14, 10, 0}, {33, 10, 0}, {34, 10, 0},
			},
			wantFireAt: 5,
		},
		{
			name: "a single qualifying frame is never enough",
			seq:  []step{{0, 5, 0}},
			// One frame proves the car was there at an instant, not that it
			// stayed — the 20 s dwell is exactly the difference.
			wantFireAt: -1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tr track
			fired := -1
			for i, s := range tt.seq {
				lat, lng := offsetNorth(s.meters)
				speed := s.mph
				got := tr.observe(Fix{
					At:        t0.Add(time.Duration(s.sec) * time.Second),
					Latitude:  lat,
					Longitude: lng,
					Speed:     &speed,
				}, pickupLat, pickupLng, cfg).dwellMet
				if got && fired == -1 {
					fired = i
				}
				if got && i < tt.wantFireAt {
					t.Fatalf("step %d (t=%ds, %.0fm, %.0fmph) fired early", i, s.sec, s.meters, s.mph)
				}
			}
			if fired != tt.wantFireAt {
				t.Errorf("first fired at step %d, want %d", fired, tt.wantFireAt)
			}
		})
	}
}

// TestTrackObserveIgnoresMilesToArrival pins the MYR-527 lesson as behaviour:
// the car's own distance-to-destination can say anything at all and change
// nothing. It is neither a trigger nor a veto.
func TestTrackObserveIgnoresMilesToArrival(t *testing.T) {
	t0 := time.Date(2026, 8, 13, 17, 0, 0, 0, time.UTC)
	cfg := DefaultConfig()
	stopped := 0.0
	near, nearLng := offsetNorth(10)
	far, farLng := offsetNorth(3000)

	tests := []struct {
		name           string
		lat, lng       float64
		milesToArrival *float64
		want           bool
	}{
		{"at the pickup, dash says 0 miles", near, nearLng, ptr(0.0), true},
		{"at the pickup, dash still says 12 miles (wrong target)", near, nearLng, ptr(12.0), true},
		{"at the pickup, dash says nothing", near, nearLng, nil, true},
		{"three km away, dash claims 0 miles", far, farLng, ptr(0.0), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tr track
			// Two frames a full dwell apart: the first opens the window, the
			// second closes it.
			first := Fix{At: t0, Latitude: tt.lat, Longitude: tt.lng, Speed: &stopped, MilesToArrival: tt.milesToArrival}
			second := first
			second.At = t0.Add(cfg.Dwell)
			tr.observe(first, pickupLat, pickupLng, cfg)
			if got := tr.observe(second, pickupLat, pickupLng, cfg).dwellMet; got != tt.want {
				t.Errorf("observe().dwellMet = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFixFrom(t *testing.T) {
	at := time.Date(2026, 8, 13, 17, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		fields  map[string]events.TelemetryValue
		wantOK  bool
		wantFix Fix
	}{
		{
			name:   "no location: nothing to measure",
			fields: map[string]events.TelemetryValue{fieldKey(telemetry.FieldSpeed): {FloatVal: ptr(0.0)}},
			wantOK: false,
		},
		{
			name: "the (0,0) no-fix sentinel is rejected",
			fields: map[string]events.TelemetryValue{
				fieldKey(telemetry.FieldLocation): {LocationVal: &events.Location{}},
			},
			wantOK: false,
		},
		{
			name: "a location the vehicle marked invalid is rejected",
			fields: map[string]events.TelemetryValue{
				fieldKey(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: pickupLat, Longitude: pickupLng}, Invalid: true},
			},
			wantOK: false,
		},
		{
			name: "location only — speed and gear stay absent, not zero",
			fields: map[string]events.TelemetryValue{
				fieldKey(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: pickupLat, Longitude: pickupLng}},
			},
			wantOK:  true,
			wantFix: Fix{At: at, Latitude: pickupLat, Longitude: pickupLng},
		},
		{
			name: "a full frame carries speed, gear and the dash's own estimate",
			fields: map[string]events.TelemetryValue{
				fieldKey(telemetry.FieldLocation):       {LocationVal: &events.Location{Latitude: pickupLat, Longitude: pickupLng}},
				fieldKey(telemetry.FieldSpeed):          {FloatVal: ptr(0.0)},
				fieldKey(telemetry.FieldGear):           {StringVal: ptrStr(gearPark)},
				fieldKey(telemetry.FieldMilesToArrival): {FloatVal: ptr(0.2)},
			},
			wantOK: true,
			wantFix: Fix{
				At: at, Latitude: pickupLat, Longitude: pickupLng,
				Speed: ptr(0.0), Gear: gearPark, MilesToArrival: ptr(0.2),
			},
		},
		{
			name: "an invalid speed reads as absent, not as zero",
			fields: map[string]events.TelemetryValue{
				fieldKey(telemetry.FieldLocation): {LocationVal: &events.Location{Latitude: pickupLat, Longitude: pickupLng}},
				fieldKey(telemetry.FieldSpeed):    {FloatVal: ptr(0.0), Invalid: true},
			},
			wantOK:  true,
			wantFix: Fix{At: at, Latitude: pickupLat, Longitude: pickupLng},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := fixFrom(events.VehicleTelemetryEvent{VIN: "VIN0", CreatedAt: at, Fields: tt.fields})
			if ok != tt.wantOK {
				t.Fatalf("fixFrom ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.At != tt.wantFix.At || got.Latitude != tt.wantFix.Latitude || got.Longitude != tt.wantFix.Longitude {
				t.Errorf("position/time = %+v, want %+v", got, tt.wantFix)
			}
			if got.Gear != tt.wantFix.Gear {
				t.Errorf("gear = %q, want %q", got.Gear, tt.wantFix.Gear)
			}
			assertFloatPtr(t, "speed", got.Speed, tt.wantFix.Speed)
			assertFloatPtr(t, "milesToArrival", got.MilesToArrival, tt.wantFix.MilesToArrival)
		})
	}
}

func fieldKey(name telemetry.FieldName) string { return string(name) }

func ptr(v float64) *float64  { return &v }
func ptrStr(v string) *string { return &v }

func assertFloatPtr(t *testing.T, label string, got, want *float64) {
	t.Helper()
	switch {
	case got == nil && want == nil:
	case got == nil || want == nil:
		t.Errorf("%s = %v, want %v", label, got, want)
	case *got != *want:
		t.Errorf("%s = %v, want %v", label, *got, *want)
	}
}
