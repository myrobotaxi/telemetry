package store

import (
	"fmt"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

func boolPtr(b bool) *bool { return &b }

func int64Ptr(i int64) *int64 { return &i }

// doorBits packs the frunk/trunk open bits the same way the decoder does, so
// the mapper test drives realistic DoorState bitmasks.
func doorBits(frunk, trunk bool) int64 {
	var bits int64
	if frunk {
		bits |= int64(events.DoorFrunk)
	}
	if trunk {
		bits |= int64(events.DoorTrunk)
	}
	return bits
}

func TestMapTelemetryToControlState(t *testing.T) {
	f := func(m map[string]events.TelemetryValue) map[string]events.TelemetryValue { return m }

	tests := []struct {
		name   string
		fields map[string]events.TelemetryValue
		want   *ControlStateUpdate // nil means expect nil return
	}{
		{
			name:   "no control fields returns nil",
			fields: f(map[string]events.TelemetryValue{string(telemetry.FieldSpeed): {FloatVal: floatPtr(42)}}),
			want:   nil,
		},
		{
			name:   "locked true",
			fields: f(map[string]events.TelemetryValue{string(telemetry.FieldLocked): {BoolVal: boolPtr(true)}}),
			want:   &ControlStateUpdate{IsLocked: boolPtr(true)},
		},
		{
			name:   "locked false is a real observation",
			fields: f(map[string]events.TelemetryValue{string(telemetry.FieldLocked): {BoolVal: boolPtr(false)}}),
			want:   &ControlStateUpdate{IsLocked: boolPtr(false)},
		},
		{
			name:   "charge port open",
			fields: f(map[string]events.TelemetryValue{string(telemetry.FieldChargePortDoorOpen): {BoolVal: boolPtr(true)}}),
			want:   &ControlStateUpdate{ChargePortOpen: boolPtr(true)},
		},
		{
			name: "doorState decodes frunk+trunk",
			fields: f(map[string]events.TelemetryValue{
				string(telemetry.FieldDoorState): {IntVal: int64Ptr(doorBits(true, false))},
			}),
			want: &ControlStateUpdate{FrunkOpen: boolPtr(true), TrunkOpen: boolPtr(false)},
		},
		{
			name: "doorState both open",
			fields: f(map[string]events.TelemetryValue{
				string(telemetry.FieldDoorState): {IntVal: int64Ptr(doorBits(true, true))},
			}),
			want: &ControlStateUpdate{FrunkOpen: boolPtr(true), TrunkOpen: boolPtr(true)},
		},
		{
			name:   "hvacPower On derives isClimateOn true",
			fields: f(map[string]events.TelemetryValue{string(telemetry.FieldHvacPower): {StringVal: strPtr("On")}}),
			want:   &ControlStateUpdate{IsClimateOn: boolPtr(true)},
		},
		{
			name:   "hvacPower Off derives isClimateOn false",
			fields: f(map[string]events.TelemetryValue{string(telemetry.FieldHvacPower): {StringVal: strPtr("Off")}}),
			want:   &ControlStateUpdate{IsClimateOn: boolPtr(false)},
		},
		{
			name:   "hvacPower Precondition derives isClimateOn true",
			fields: f(map[string]events.TelemetryValue{string(telemetry.FieldHvacPower): {StringVal: strPtr("Precondition")}}),
			want:   &ControlStateUpdate{IsClimateOn: boolPtr(true)},
		},
		{
			name:   "hvacPower Unknown omits isClimateOn (nil, not fabricated)",
			fields: f(map[string]events.TelemetryValue{string(telemetry.FieldHvacPower): {StringVal: strPtr("Unknown")}}),
			want:   nil, // only field present was omitted → whole update is nil
		},
		{
			name: "invalid field is ignored",
			fields: f(map[string]events.TelemetryValue{
				string(telemetry.FieldLocked): {BoolVal: boolPtr(true), Invalid: true},
			}),
			want: nil,
		},
		{
			name: "all four controls together",
			fields: f(map[string]events.TelemetryValue{
				string(telemetry.FieldLocked):             {BoolVal: boolPtr(true)},
				string(telemetry.FieldDoorState):          {IntVal: int64Ptr(doorBits(false, true))},
				string(telemetry.FieldHvacPower):          {StringVal: strPtr("On")},
				string(telemetry.FieldChargePortDoorOpen): {BoolVal: boolPtr(false)},
			}),
			want: &ControlStateUpdate{
				IsLocked:       boolPtr(true),
				FrunkOpen:      boolPtr(false),
				TrunkOpen:      boolPtr(true),
				IsClimateOn:    boolPtr(true),
				ChargePortOpen: boolPtr(false),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapTelemetryToControlState(tt.fields)
			assertControlEqual(t, tt.want, got)
		})
	}
}

func TestMergeControlState_PerFieldLastWriteWins(t *testing.T) {
	dst := &ControlStateUpdate{IsLocked: boolPtr(true), FrunkOpen: boolPtr(false)}
	src := &ControlStateUpdate{FrunkOpen: boolPtr(true), ChargePortOpen: boolPtr(true)}
	mergeControlState(dst, src)

	// IsLocked kept from dst (src nil), FrunkOpen overwritten by src, ChargePortOpen added.
	assertControlEqual(t, &ControlStateUpdate{
		IsLocked:       boolPtr(true),
		FrunkOpen:      boolPtr(true),
		ChargePortOpen: boolPtr(true),
	}, dst)
}

func TestControlStateUpdate_HasAny(t *testing.T) {
	if (&ControlStateUpdate{}).HasAny() {
		t.Error("empty ControlStateUpdate should have no fields")
	}
	if (*ControlStateUpdate)(nil).HasAny() {
		t.Error("nil ControlStateUpdate should have no fields")
	}
	if !(&ControlStateUpdate{IsLocked: boolPtr(false)}).HasAny() {
		t.Error("a present false field should count as HasAny")
	}
}

// assertControlEqual compares two *ControlStateUpdate by value (nil-safe).
func assertControlEqual(t *testing.T, want, got *ControlStateUpdate) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Fatalf("want nil, got %+v", derefControl(got))
		}
		return
	}
	if got == nil {
		t.Fatalf("want %+v, got nil", derefControl(want))
	}
	eqBoolPtr(t, "IsLocked", want.IsLocked, got.IsLocked)
	eqBoolPtr(t, "FrunkOpen", want.FrunkOpen, got.FrunkOpen)
	eqBoolPtr(t, "TrunkOpen", want.TrunkOpen, got.TrunkOpen)
	eqBoolPtr(t, "IsClimateOn", want.IsClimateOn, got.IsClimateOn)
	eqBoolPtr(t, "ChargePortOpen", want.ChargePortOpen, got.ChargePortOpen)
}

func eqBoolPtr(t *testing.T, field string, want, got *bool) {
	t.Helper()
	switch {
	case want == nil && got == nil:
		return
	case want == nil || got == nil:
		t.Errorf("%s: want %v, got %v", field, fmtBoolPtr(want), fmtBoolPtr(got))
	case *want != *got:
		t.Errorf("%s: want %v, got %v", field, *want, *got)
	}
}

func fmtBoolPtr(p *bool) string {
	if p == nil {
		return "nil"
	}
	if *p {
		return "true"
	}
	return "false"
}

func derefControl(c *ControlStateUpdate) string {
	if c == nil {
		return "nil"
	}
	return "{" +
		"IsLocked:" + fmtBoolPtr(c.IsLocked) +
		" FrunkOpen:" + fmtBoolPtr(c.FrunkOpen) +
		" TrunkOpen:" + fmtBoolPtr(c.TrunkOpen) +
		" IsClimateOn:" + fmtBoolPtr(c.IsClimateOn) +
		" ChargePortOpen:" + fmtBoolPtr(c.ChargePortOpen) + "}"
}

// TestMapTelemetryToControlState_CabinLevels covers the MYR-273 cabin-setting
// derivation: temp setpoints, fan speed, seat heater/cooler levels arrive as
// either IntVal or FloatVal (a float is rounded to nearest int, matching the WS
// roundIfInteger); mediaVolume stays fractional (never rounded); an Invalid
// field is ignored.
func TestMapTelemetryToControlState_CabinLevels(t *testing.T) {
	t.Run("int level from IntVal", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldSeatHeaterLeft): {IntVal: int64Ptr(3)},
		})
		eqIntPtr(t, "SeatHeaterLeft", intPtr(3), got.SeatHeaterLeft)
	})

	t.Run("int level from FloatVal is rounded", func(t *testing.T) {
		// Temp setpoints flow through convertTemperature → FloatVal (Fahrenheit).
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldDriverTempSetting): {FloatVal: floatPtr(68.4)},
			string(telemetry.FieldHvacFanSpeed):      {FloatVal: floatPtr(2.5)},
		})
		eqIntPtr(t, "DriverTempSetting", intPtr(68), got.DriverTempSetting)
		eqIntPtr(t, "FanSpeed (2.5 rounds to 3)", intPtr(3), got.FanSpeed)
	})

	t.Run("media volume stays fractional", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldMediaVolume): {FloatVal: floatPtr(5.5)},
		})
		eqFloatPtr(t, "MediaVolume", floatPtr(5.5), got.MediaVolume)
	})

	t.Run("media volume zero is a real observation", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldMediaVolume): {FloatVal: floatPtr(0)},
		})
		eqFloatPtr(t, "MediaVolume", floatPtr(0), got.MediaVolume)
	})

	t.Run("invalid level field is ignored", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldSeatCoolerLeft): {IntVal: int64Ptr(2), Invalid: true},
		})
		if got != nil {
			t.Fatalf("invalid-only frame should map to nil, got %+v", got)
		}
	})

	t.Run("all cabin levels together", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldDriverTempSetting):    {IntVal: int64Ptr(68)},
			string(telemetry.FieldPassengerTempSetting): {IntVal: int64Ptr(70)},
			string(telemetry.FieldHvacFanSpeed):         {IntVal: int64Ptr(3)},
			string(telemetry.FieldSeatHeaterLeft):       {IntVal: int64Ptr(2)},
			string(telemetry.FieldSeatHeaterRight):      {IntVal: int64Ptr(1)},
			string(telemetry.FieldSeatHeaterRearLeft):   {IntVal: int64Ptr(0)},
			string(telemetry.FieldSeatHeaterRearCenter): {IntVal: int64Ptr(0)},
			string(telemetry.FieldSeatHeaterRearRight):  {IntVal: int64Ptr(0)},
			string(telemetry.FieldSeatCoolerLeft):       {IntVal: int64Ptr(1)},
			string(telemetry.FieldSeatCoolerRight):      {IntVal: int64Ptr(2)},
			string(telemetry.FieldMediaVolume):          {FloatVal: floatPtr(7.5)},
		})
		eqIntPtr(t, "DriverTempSetting", intPtr(68), got.DriverTempSetting)
		eqIntPtr(t, "PassengerTempSetting", intPtr(70), got.PassengerTempSetting)
		eqIntPtr(t, "FanSpeed", intPtr(3), got.FanSpeed)
		eqIntPtr(t, "SeatHeaterLeft", intPtr(2), got.SeatHeaterLeft)
		eqIntPtr(t, "SeatHeaterRight", intPtr(1), got.SeatHeaterRight)
		eqIntPtr(t, "SeatHeaterRearLeft", intPtr(0), got.SeatHeaterRearLeft)
		eqIntPtr(t, "SeatHeaterRearCenter", intPtr(0), got.SeatHeaterRearCenter)
		eqIntPtr(t, "SeatHeaterRearRight", intPtr(0), got.SeatHeaterRearRight)
		eqIntPtr(t, "SeatCoolerLeft", intPtr(1), got.SeatCoolerLeft)
		eqIntPtr(t, "SeatCoolerRight", intPtr(2), got.SeatCoolerRight)
		eqFloatPtr(t, "MediaVolume", floatPtr(7.5), got.MediaVolume)
	})
}

// TestMergeControlState_CabinLevelsLastWriteWins proves per-field last-writer-
// wins folds the MYR-273 levels too: a present src level overwrites, an absent
// src level preserves the prior dst value.
func TestMergeControlState_CabinLevelsLastWriteWins(t *testing.T) {
	dst := &ControlStateUpdate{
		FanSpeed:       intPtr(2),
		SeatHeaterLeft: intPtr(1),
		MediaVolume:    floatPtr(3.0),
	}
	src := &ControlStateUpdate{
		FanSpeed:        intPtr(4),     // overwrite
		MediaVolume:     floatPtr(6.5), // overwrite
		SeatCoolerRight: intPtr(2),     // new
		// SeatHeaterLeft absent in src → preserved from dst
	}
	mergeControlState(dst, src)

	eqIntPtr(t, "FanSpeed (overwritten)", intPtr(4), dst.FanSpeed)
	eqIntPtr(t, "SeatHeaterLeft (preserved)", intPtr(1), dst.SeatHeaterLeft)
	eqIntPtr(t, "SeatCoolerRight (new)", intPtr(2), dst.SeatCoolerRight)
	eqFloatPtr(t, "MediaVolume (overwritten)", floatPtr(6.5), dst.MediaVolume)
}

// eqIntPtr compares two *int (nil-safe).
func eqIntPtr(t *testing.T, field string, want, got *int) {
	t.Helper()
	switch {
	case want == nil && got == nil:
		return
	case want == nil || got == nil:
		t.Errorf("%s: want %v, got %v", field, fmtIntPtr(want), fmtIntPtr(got))
	case *want != *got:
		t.Errorf("%s: want %d, got %d", field, *want, *got)
	}
}

// eqFloatPtr compares two *float64 (nil-safe, exact).
func eqFloatPtr(t *testing.T, field string, want, got *float64) {
	t.Helper()
	switch {
	case want == nil && got == nil:
		return
	case want == nil || got == nil:
		t.Errorf("%s: want %v, got %v", field, want, got)
	case *want != *got:
		t.Errorf("%s: want %v, got %v", field, *want, *got)
	}
}

func fmtIntPtr(p *int) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprintf("%d", *p)
}

// TestMapTelemetryToControlState_VehicleDetailStrings covers the MYR-279
// vehicle-detail derivation: software version (streamed Version OR /vehicle_data
// car_version) and trim (/vehicle_data-only) map to the string fields; empty or
// Invalid strings are ignored so a blank frame never overwrites a known value.
func TestMapTelemetryToControlState_VehicleDetailStrings(t *testing.T) {
	t.Run("software version and trim map to string fields", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldVersion): {StringVal: strPtr("2026.20.1")},
			string(telemetry.FieldTrim):    {StringVal: strPtr("Performance")},
		})
		if got == nil {
			t.Fatal("expected non-nil control state")
		}
		if got.SoftwareVersion == nil || *got.SoftwareVersion != "2026.20.1" {
			t.Errorf("SoftwareVersion = %v, want 2026.20.1", got.SoftwareVersion)
		}
		if got.Trim == nil || *got.Trim != "Performance" {
			t.Errorf("Trim = %v, want Performance", got.Trim)
		}
	})

	t.Run("empty strings are ignored", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldVersion): {StringVal: strPtr("")},
			string(telemetry.FieldTrim):    {StringVal: strPtr("")},
		})
		if got != nil {
			t.Fatalf("empty-only frame should map to nil, got %+v", got)
		}
	})

	t.Run("invalid version is ignored", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldVersion): {StringVal: strPtr("2026.20.1"), Invalid: true},
		})
		if got != nil {
			t.Fatalf("invalid-only frame should map to nil, got %+v", got)
		}
	})
}

// TestMapTelemetryToControlState_ClimateMode covers the MYR-274 climate-mode
// derivation backing the owner Auto/Cool/Heat segment: hvacAutoMode maps to the
// string field (enum's "On"/"Override" form) and hvacAcEnabled to the bool field;
// a streamed "Unknown" (or empty) hvacAutoMode is OMITTED (honest-unknown, never a
// fabricated mode), mirroring how an "Unknown" hvacPower omits isClimateOn; an
// Invalid field is ignored; a real false hvacAcEnabled persists.
func TestMapTelemetryToControlState_ClimateMode(t *testing.T) {
	t.Run("hvacAutoMode On maps to string field", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldHvacAutoMode): {StringVal: strPtr("On")},
		})
		if got == nil || got.HvacAutoMode == nil || *got.HvacAutoMode != "On" {
			t.Fatalf("HvacAutoMode = %v, want On", got)
		}
	})

	t.Run("hvacAutoMode Override maps to string field", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldHvacAutoMode): {StringVal: strPtr("Override")},
		})
		if got == nil || got.HvacAutoMode == nil || *got.HvacAutoMode != "Override" {
			t.Fatalf("HvacAutoMode = %v, want Override", got)
		}
	})

	t.Run("hvacAutoMode Unknown is omitted (honest-unknown)", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldHvacAutoMode): {StringVal: strPtr("Unknown")},
		})
		if got != nil {
			t.Fatalf("Unknown-only frame should map to nil, got %+v", got)
		}
	})

	t.Run("hvacAutoMode empty is omitted", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldHvacAutoMode): {StringVal: strPtr("")},
		})
		if got != nil {
			t.Fatalf("empty-only frame should map to nil, got %+v", got)
		}
	})

	t.Run("hvacAcEnabled true maps to bool field", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldHvacACEnabled): {BoolVal: boolPtr(true)},
		})
		if got == nil || got.HvacAcEnabled == nil || !*got.HvacAcEnabled {
			t.Fatalf("HvacAcEnabled = %v, want true", got)
		}
	})

	t.Run("hvacAcEnabled false is a real observation", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldHvacACEnabled): {BoolVal: boolPtr(false)},
		})
		if got == nil || got.HvacAcEnabled == nil || *got.HvacAcEnabled {
			t.Fatalf("HvacAcEnabled = %v, want false", got)
		}
	})

	t.Run("invalid climate-mode fields are ignored", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldHvacAutoMode):  {StringVal: strPtr("Override"), Invalid: true},
			string(telemetry.FieldHvacACEnabled): {BoolVal: boolPtr(true), Invalid: true},
		})
		if got != nil {
			t.Fatalf("invalid-only frame should map to nil, got %+v", got)
		}
	})

	t.Run("Override + acEnabled together", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldHvacAutoMode):  {StringVal: strPtr("Override")},
			string(telemetry.FieldHvacACEnabled): {BoolVal: boolPtr(true)},
		})
		if got == nil || got.HvacAutoMode == nil || *got.HvacAutoMode != "Override" {
			t.Fatalf("HvacAutoMode = %v, want Override", got)
		}
		if got.HvacAcEnabled == nil || !*got.HvacAcEnabled {
			t.Fatalf("HvacAcEnabled = %v, want true", got)
		}
	})
}

// TestMergeControlState_ClimateModeLastWriteWins proves per-field last-writer-wins
// folds the MYR-274 climate-mode fields: a present src value overwrites, an absent
// src value preserves the prior dst value.
func TestMergeControlState_ClimateModeLastWriteWins(t *testing.T) {
	dst := &ControlStateUpdate{HvacAutoMode: strPtr("On"), HvacAcEnabled: boolPtr(true)}
	src := &ControlStateUpdate{HvacAutoMode: strPtr("Override")} // acEnabled absent → preserved
	mergeControlState(dst, src)

	if dst.HvacAutoMode == nil || *dst.HvacAutoMode != "Override" {
		t.Errorf("HvacAutoMode (overwritten) = %v, want Override", dst.HvacAutoMode)
	}
	if dst.HvacAcEnabled == nil || !*dst.HvacAcEnabled {
		t.Errorf("HvacAcEnabled (preserved) = %v, want true", dst.HvacAcEnabled)
	}
}

// TestMapTelemetryToControlState_SeatVentMedia covers the MYR-298 derivation:
// seatVentEnabled is a plain bool (a real false is a real observation), and
// mediaPlaybackStatus carries the enum string but OMITS "Unknown"/empty so a
// genuinely-unknown status never overwrites a known one — the same
// honest-unknown discipline MYR-274 applies to hvacAutoMode.
func TestMapTelemetryToControlState_SeatVentMedia(t *testing.T) {
	t.Run("seatVentEnabled true maps to bool field", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldSeatVentEnabled): {BoolVal: boolPtr(true)},
		})
		if got == nil || got.SeatVentEnabled == nil || !*got.SeatVentEnabled {
			t.Fatalf("SeatVentEnabled = %v, want true", got)
		}
	})

	t.Run("seatVentEnabled false is a real observation", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldSeatVentEnabled): {BoolVal: boolPtr(false)},
		})
		if got == nil || got.SeatVentEnabled == nil || *got.SeatVentEnabled {
			t.Fatalf("SeatVentEnabled = %v, want false", got)
		}
	})

	for _, status := range []string{"Playing", "Paused", "Stopped"} {
		t.Run("mediaPlaybackStatus "+status+" maps to string field", func(t *testing.T) {
			got := mapTelemetryToControlState(map[string]events.TelemetryValue{
				string(telemetry.FieldMediaPlaybackStatus): {StringVal: strPtr(status)},
			})
			if got == nil || got.MediaPlaybackStatus == nil || *got.MediaPlaybackStatus != status {
				t.Fatalf("MediaPlaybackStatus = %v, want %s", got, status)
			}
		})
	}

	t.Run("mediaPlaybackStatus Unknown is omitted (honest-unknown)", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldMediaPlaybackStatus): {StringVal: strPtr("Unknown")},
		})
		if got != nil {
			t.Fatalf("Unknown-only frame should map to nil, got %+v", got)
		}
	})

	t.Run("mediaPlaybackStatus empty is omitted", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldMediaPlaybackStatus): {StringVal: strPtr("")},
		})
		if got != nil {
			t.Fatalf("empty-only frame should map to nil, got %+v", got)
		}
	})

	t.Run("invalid seat-vent/media fields are ignored", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldSeatVentEnabled):     {BoolVal: boolPtr(true), Invalid: true},
			string(telemetry.FieldMediaPlaybackStatus): {StringVal: strPtr("Playing"), Invalid: true},
		})
		if got != nil {
			t.Fatalf("invalid-only frame should map to nil, got %+v", got)
		}
	})

	t.Run("seat vent + media together", func(t *testing.T) {
		got := mapTelemetryToControlState(map[string]events.TelemetryValue{
			string(telemetry.FieldSeatVentEnabled):     {BoolVal: boolPtr(true)},
			string(telemetry.FieldMediaPlaybackStatus): {StringVal: strPtr("Paused")},
		})
		if got == nil || got.SeatVentEnabled == nil || !*got.SeatVentEnabled {
			t.Fatalf("SeatVentEnabled = %v, want true", got)
		}
		if got.MediaPlaybackStatus == nil || *got.MediaPlaybackStatus != "Paused" {
			t.Fatalf("MediaPlaybackStatus = %v, want Paused", got)
		}
	})
}

// TestMergeControlState_SeatVentMediaLastWriteWins proves per-field
// last-writer-wins folds the MYR-298 fields: a present src value overwrites, an
// absent src value preserves the prior dst value.
func TestMergeControlState_SeatVentMediaLastWriteWins(t *testing.T) {
	dst := &ControlStateUpdate{SeatVentEnabled: boolPtr(true), MediaPlaybackStatus: strPtr("Playing")}
	src := &ControlStateUpdate{MediaPlaybackStatus: strPtr("Paused")} // seat vent absent → preserved
	mergeControlState(dst, src)

	if dst.MediaPlaybackStatus == nil || *dst.MediaPlaybackStatus != "Paused" {
		t.Errorf("MediaPlaybackStatus (overwritten) = %v, want Paused", dst.MediaPlaybackStatus)
	}
	if dst.SeatVentEnabled == nil || !*dst.SeatVentEnabled {
		t.Errorf("SeatVentEnabled (preserved) = %v, want true", dst.SeatVentEnabled)
	}
}
