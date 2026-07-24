package ws

import (
	"testing"

	"github.com/myrobotaxi/telemetry/internal/events"
)

func strVal(s string) events.TelemetryValue  { return events.TelemetryValue{StringVal: &s} }
func intTVal(i int64) events.TelemetryValue  { return events.TelemetryValue{IntVal: &i} }
func fltVal(f float64) events.TelemetryValue { return events.TelemetryValue{FloatVal: &f} }
func boolTVal(b bool) events.TelemetryValue  { return events.TelemetryValue{BoolVal: &b} }

// TestMapFields_DoorStateSplit verifies the DoorState bitmask (MYR-252) is
// unpacked into the frunkOpen/trunkOpen wire booleans and the raw doorState
// field is NOT surfaced.
func TestMapFields_DoorStateSplit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		bits      int64
		wantFrunk bool
		wantTrunk bool
	}{
		{"all closed", 0, false, false},
		{"frunk only", int64(events.DoorFrunk), true, false},
		{"trunk only", int64(events.DoorTrunk), false, true},
		{"both", int64(events.DoorFrunk) | int64(events.DoorTrunk), true, true},
		// Other doors set but not frunk/trunk → both false.
		{"cabin doors only", int64(events.DoorDriverFront) | int64(events.DoorPassengerRear), false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := mapFieldsForClient(map[string]events.TelemetryValue{
				"doorState": intTVal(tt.bits),
			})
			if _, ok := out["doorState"]; ok {
				t.Error("raw doorState leaked onto the wire; expected only frunkOpen/trunkOpen")
			}
			if out["frunkOpen"] != tt.wantFrunk {
				t.Errorf("frunkOpen = %v, want %v", out["frunkOpen"], tt.wantFrunk)
			}
			if out["trunkOpen"] != tt.wantTrunk {
				t.Errorf("trunkOpen = %v, want %v", out["trunkOpen"], tt.wantTrunk)
			}
		})
	}
}

// TestMapFields_IsClimateOn verifies the MYR-252 case-insensitive fix: an
// "Off" hvacPower (capitalized, as hvacPowerString emits) yields
// isClimateOn=false, not the pre-fix always-true bug.
func TestMapFields_IsClimateOn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		power   string
		want    bool
		present bool // whether isClimateOn should be emitted at all
	}{
		{"Off", false, true},
		{"On", true, true},
		{"Precondition", true, true},
		{"OverheatProtect", true, true},
		// Honest unknown: isClimateOn must be OMITTED, never asserted true.
		{"Unknown", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.power, func(t *testing.T) {
			t.Parallel()
			out := mapFieldsForClient(map[string]events.TelemetryValue{
				"hvacPower": strVal(tt.power),
			})
			got, ok := out["isClimateOn"]
			if ok != tt.present {
				t.Errorf("isClimateOn presence for hvacPower=%q = %v, want present=%v (got value %v)", tt.power, ok, tt.present, got)
			}
			if tt.present && got != tt.want {
				t.Errorf("isClimateOn for hvacPower=%q = %v, want %v", tt.power, got, tt.want)
			}
			if out["hvacPower"] != tt.power {
				t.Errorf("hvacPower = %v, want %q (should pass through unchanged)", out["hvacPower"], tt.power)
			}
		})
	}
}

// TestMapFields_CabinPassthroughAndRounding verifies Group A+B fields reach
// the wire under their contract names, integer seat levels are rounded, and
// mediaVolume stays a float.
func TestMapFields_CabinPassthroughAndRounding(t *testing.T) {
	t.Parallel()

	out := mapFieldsForClient(map[string]events.TelemetryValue{
		"locked":              boolTVal(true),
		"hvacAcEnabled":       boolTVal(true),
		"seatVentEnabled":     boolTVal(false),
		"chargePortDoorOpen":  boolTVal(true),
		"hvacAutoMode":        strVal("On"),
		"mediaPlaybackStatus": strVal("Playing"),
		"hvacFanSpeed":        fltVal(3.0), // internal name → wire "fanSpeed"
		"seatHeaterRearLeft":  fltVal(2.0),
		"seatCoolerRight":     fltVal(1.0),
		"mediaVolume":         fltVal(5.5),
	})

	checks := map[string]any{
		"locked":              true,
		"hvacAcEnabled":       true,
		"seatVentEnabled":     false,
		"chargePortDoorOpen":  true,
		"hvacAutoMode":        "On",
		"mediaPlaybackStatus": "Playing",
		"fanSpeed":            3,   // rounded to int
		"seatHeaterRearLeft":  2,   // rounded to int
		"seatCoolerRight":     1,   // rounded to int
		"mediaVolume":         5.5, // NOT rounded — stays float
	}
	for k, want := range checks {
		if out[k] != want {
			t.Errorf("out[%q] = %#v, want %#v", k, out[k], want)
		}
	}
	if _, ok := out["hvacFanSpeed"]; ok {
		t.Error("internal name hvacFanSpeed leaked; expected wire name fanSpeed")
	}
}
