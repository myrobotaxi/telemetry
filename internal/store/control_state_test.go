package store

import (
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
