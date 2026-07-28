package store

import (
	"testing"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
)

// TestMapControlStrings_MYR320 covers the field mapper for all FOUR
// vehicle-detail strings, because MYR-320 made them table-driven and a table is
// exactly where a wrong entry hides: transposing two same-typed targets would
// put the FSD designation in the firmware column and nothing would complain.
//
// The empty-string DROP is the rule that matters. These four are facts about the
// car that only ever become better-known, so an empty read means "we learned
// nothing" and must leave the stored value alone. That is the deliberate
// opposite of the MYR-303 media text fields, where an empty value is a real
// observation ("the track ended") that has to overwrite.
func TestMapControlStrings_MYR320(t *testing.T) {
	sv := func(s string) events.TelemetryValue { return events.TelemetryValue{StringVal: &s} }

	tests := []struct {
		name        string
		fields      map[string]events.TelemetryValue
		wantVersion *string
		wantTrim    *string
		wantLabel   *string
		wantFSD     *string
	}{
		{
			name: "all four map to their own targets",
			fields: map[string]events.TelemetryValue{
				string(telemetry.FieldVersion):    sv("2026.20.1 9a8b7c6"),
				string(telemetry.FieldTrim):       sv("p74d"),
				string(telemetry.FieldTrimLabel):  sv("Performance"),
				string(telemetry.FieldFSDVersion): sv("FSD (Supervised) v14.3.5"),
			},
			wantVersion: strp("2026.20.1 9a8b7c6"),
			wantTrim:    strp("p74d"),
			wantLabel:   strp("Performance"),
			wantFSD:     strp("FSD (Supervised) v14.3.5"),
		},
		{
			// The label must not be mistaken for the badge code, nor vice versa.
			name: "label and badge stay in their own columns",
			fields: map[string]events.TelemetryValue{
				string(telemetry.FieldTrim):      sv("p74d"),
				string(telemetry.FieldTrimLabel): sv("Performance"),
			},
			wantTrim:  strp("p74d"),
			wantLabel: strp("Performance"),
		},
		{
			// Likewise the FSD designation must not land in software_version.
			name: "FSD version and firmware build stay in their own columns",
			fields: map[string]events.TelemetryValue{
				string(telemetry.FieldVersion):    sv("2026.20.1 9a8b7c6"),
				string(telemetry.FieldFSDVersion): sv("FSD (Supervised) v14.3.5"),
			},
			wantVersion: strp("2026.20.1 9a8b7c6"),
			wantFSD:     strp("FSD (Supervised) v14.3.5"),
		},
		{
			name: "EMPTY strings are dropped so they cannot blank a known value",
			fields: map[string]events.TelemetryValue{
				string(telemetry.FieldTrimLabel):  sv(""),
				string(telemetry.FieldFSDVersion): sv(""),
			},
		},
		{
			name: "INVALID values are dropped",
			fields: map[string]events.TelemetryValue{
				string(telemetry.FieldTrimLabel):  {StringVal: strp("Performance"), Invalid: true},
				string(telemetry.FieldFSDVersion): {StringVal: strp("FSD (Supervised) v14.3.5"), Invalid: true},
			},
		},
		{
			name: "non-string values are dropped",
			fields: map[string]events.TelemetryValue{
				string(telemetry.FieldTrimLabel):  {IntVal: int64Ptr(7)},
				string(telemetry.FieldFSDVersion): {},
			},
		},
		{
			name:   "absent fields leave everything nil",
			fields: map[string]events.TelemetryValue{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got ControlStateUpdate
			mapControlStrings(tt.fields, &got)

			wantStringPtr(t, "SoftwareVersion", tt.wantVersion, got.SoftwareVersion)
			wantStringPtr(t, "Trim", tt.wantTrim, got.Trim)
			wantStringPtr(t, "TrimLabel", tt.wantLabel, got.TrimLabel)
			wantStringPtr(t, "FSDVersion", tt.wantFSD, got.FSDVersion)
		})
	}
}

// A frame carrying ONLY a MYR-320 detail string must still trigger the
// side-table upsert. HasAny gates the write, so a missed entry there would make
// the whole feature a silent no-op for the commonest shape: an in-service car
// whose release_notes read succeeded and whose control fields did not change.
func TestControlStateUpdate_HasAnyCoversMYR320Details(t *testing.T) {
	tests := []struct {
		name   string
		update ControlStateUpdate
	}{
		{name: "trim label alone", update: ControlStateUpdate{TrimLabel: strp("Performance")}},
		{name: "FSD version alone", update: ControlStateUpdate{FSDVersion: strp("FSD (Supervised) v14.3.5")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !tt.update.HasAny() {
				t.Error("HasAny = false — the upsert would be skipped and the value lost")
			}
		})
	}
	if (&ControlStateUpdate{}).HasAny() {
		t.Error("HasAny = true for an empty update, want false")
	}
}

// The writer's coalescing buffer merges frames before the upsert, so a detail
// string arriving in one frame must survive a later frame that carries other
// fields — otherwise a busy car would keep discarding its own details.
func TestMergeControlState_MYR320Details(t *testing.T) {
	dst := &ControlStateUpdate{TrimLabel: strp("Performance")}
	src := &ControlStateUpdate{FSDVersion: strp("FSD (Supervised) v14.3.5")}

	mergeControlState(dst, src)

	wantStringPtr(t, "TrimLabel", strp("Performance"), dst.TrimLabel)
	wantStringPtr(t, "FSDVersion", strp("FSD (Supervised) v14.3.5"), dst.FSDVersion)

	// Latest wins per field, like every other column.
	newer := &ControlStateUpdate{TrimLabel: strp("Long Range")}
	mergeControlState(dst, newer)
	wantStringPtr(t, "TrimLabel", strp("Long Range"), dst.TrimLabel)
	wantStringPtr(t, "FSDVersion", strp("FSD (Supervised) v14.3.5"), dst.FSDVersion)
}

func strp(s string) *string { return &s }

func wantStringPtr(t *testing.T, field string, want, got *string) {
	t.Helper()
	switch {
	case want == nil && got == nil:
	case want == nil:
		t.Errorf("%s = %q, want nil", field, *got)
	case got == nil:
		t.Errorf("%s = nil, want %q", field, *want)
	case *want != *got:
		t.Errorf("%s = %q, want %q", field, *got, *want)
	}
}
