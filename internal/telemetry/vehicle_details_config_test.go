package telemetry

import (
	"encoding/json"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/events"
)

// TestVehicleConfigDecodesMYR320Fields covers the MYR-320 vehicle_config decode.
// The distinctions that matter and MUST stay distinct:
//
//	performance_package present  → the DISPLAY-SAFE trim label ("Performance")
//	trim_badging present         → the RAW BADGE CODE ("p74d"), a different value
//	exterior_color present       → Tesla's colour name ("Quicksilver")
//	any of them absent           → nil, NOT the empty string
//
// The absent-vs-empty distinction is the failure this test exists to prevent:
// collapsing them would let a partial Tesla payload blank a label or a colour an
// earlier read got right.
func TestVehicleConfigDecodesMYR320Fields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		body          string
		wantTrim      *string
		wantLabel     *string
		wantColor     *string
		wantNilConfig bool
	}{
		{
			name: "live-verified shape: badge code and label coexist",
			body: `{"response":{"vehicle_config":{"trim_badging":"p74d",` +
				`"performance_package":"Performance","exterior_color":"Quicksilver"}}}`,
			wantTrim:  ptr("p74d"),
			wantLabel: ptr("Performance"),
			wantColor: ptr("Quicksilver"),
		},
		{
			name:      "performance_package absent decodes to nil, not empty",
			body:      `{"response":{"vehicle_config":{"trim_badging":"p74d","exterior_color":"Quicksilver"}}}`,
			wantTrim:  ptr("p74d"),
			wantColor: ptr("Quicksilver"),
		},
		{
			name:      "exterior_color absent decodes to nil, not empty",
			body:      `{"response":{"vehicle_config":{"performance_package":"Performance"}}}`,
			wantLabel: ptr("Performance"),
		},
		{
			name: "explicit empty strings decode as empty, distinct from absent",
			body: `{"response":{"vehicle_config":{"performance_package":"","exterior_color":""}}}`,
			// Present-but-empty is preserved by the decoder; the MAPPER is what
			// drops it (see TestAddVehicleConfigFieldsTrimLabel). Keeping the
			// distinction here means the skip rule lives in exactly one place.
			wantLabel: ptr(""),
			wantColor: ptr(""),
		},
		{
			name:          "absent vehicle_config object entirely",
			body:          `{"response":{"vehicle_state":{"locked":true}}}`,
			wantNilConfig: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got vehicleDataResponse
			if err := json.Unmarshal([]byte(tt.body), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			vc := got.Response.VehicleConfig
			if tt.wantNilConfig {
				if vc != nil {
					t.Fatalf("vehicle_config = %+v, want nil", vc)
				}
				return
			}
			if vc == nil {
				t.Fatal("vehicle_config = nil, want decoded object")
			}
			assertStrPtr(t, "trim_badging", vc.TrimBadging, tt.wantTrim)
			assertStrPtr(t, "performance_package", vc.PerformancePackage, tt.wantLabel)
			assertStrPtr(t, "exterior_color", vc.ExteriorColor, tt.wantColor)
		})
	}
}

// TestAddVehicleConfigFieldsTrimLabel pins the mapper's two MYR-320 rules:
// performance_package becomes the trimLabel telemetry field ALONGSIDE trim (it
// never replaces it), and exterior_color is deliberately NOT mapped as a
// telemetry field at all — it takes the column-write path instead, because the
// wire already has `color`.
func TestAddVehicleConfigFieldsTrimLabel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		config    *VehicleDataVehicleConfig
		wantTrim  string // "" means the field must be ABSENT
		wantLabel string
	}{
		{
			name: "badge and label both map, independently",
			config: &VehicleDataVehicleConfig{
				TrimBadging:        ptr("p74d"),
				PerformancePackage: ptr("Performance"),
			},
			wantTrim:  "p74d",
			wantLabel: "Performance",
		},
		{
			name:      "label alone still maps",
			config:    &VehicleDataVehicleConfig{PerformancePackage: ptr("Performance")},
			wantLabel: "Performance",
		},
		{
			name:     "empty label is SKIPPED so it cannot blank a known one",
			config:   &VehicleDataVehicleConfig{TrimBadging: ptr("p74d"), PerformancePackage: ptr("")},
			wantTrim: "p74d",
		},
		{
			name:     "absent label is skipped",
			config:   &VehicleDataVehicleConfig{TrimBadging: ptr("p74d")},
			wantTrim: "p74d",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			fields := make(map[string]events.TelemetryValue)
			addVehicleConfigFields(fields, tt.config)

			assertTelemetryString(t, fields, string(FieldTrim), tt.wantTrim)
			assertTelemetryString(t, fields, string(FieldTrimLabel), tt.wantLabel)

			// exterior_color must NEVER become a telemetry field: it has an
			// existing wire field fed by an existing column, so routing it here
			// would mean inventing a second name for a value already shipped.
			if _, ok := fields["color"]; ok {
				t.Error("exterior_color leaked into the telemetry frame as `color`")
			}
			if _, ok := fields["exteriorColor"]; ok {
				t.Error("exterior_color leaked into the telemetry frame as `exteriorColor`")
			}
		})
	}
}

// TestVehicleDataToFieldsCarriesTrimLabel proves the label survives the whole
// mapper (not just addVehicleConfigFields) and that it stays out of
// streamSourcedFields — the property that carries it past the MYR-300
// stream-recency gate, which is what lets an in-service car (never streaming)
// acquire it at all.
func TestVehicleDataToFieldsCarriesTrimLabel(t *testing.T) {
	t.Parallel()

	fields := vehicleDataToFields(&VehicleData{
		VehicleConfig: &VehicleDataVehicleConfig{PerformancePackage: ptr("Performance")},
	})
	assertTelemetryString(t, fields, string(FieldTrimLabel), "Performance")

	for _, name := range []FieldName{FieldTrimLabel, FieldFSDVersion} {
		if _, streamed := streamSourcedFields[string(name)]; streamed {
			t.Errorf("%s is in streamSourcedFields — the MYR-300 gate would drop it, "+
				"and a REST-only field has no stream source to be stale against", name)
		}
	}

	// The gate must actually pass them through, not merely fail to list them.
	gated := map[string]events.TelemetryValue{
		string(FieldTrimLabel):  {StringVal: ptr("Performance")},
		string(FieldFSDVersion): {StringVal: ptr("FSD (Supervised) v14.3.5")},
		string(FieldSOC):        {FloatVal: ptrFloat(55)},
	}
	if dropped := dropStreamSourcedFields(gated); dropped != 1 {
		t.Errorf("dropped = %d, want 1 (only the streamed soc)", dropped)
	}
	assertTelemetryString(t, gated, string(FieldTrimLabel), "Performance")
	assertTelemetryString(t, gated, string(FieldFSDVersion), "FSD (Supervised) v14.3.5")
}

// --- helpers -------------------------------------------------------------

func ptr(s string) *string        { return &s }
func ptrFloat(f float64) *float64 { return &f }

// assertTelemetryString asserts a field's string value, or its ABSENCE when
// want is "". Absence is the assertion that matters most here: the skip rules
// are what stop a blank read from overwriting a good value.
func assertTelemetryString(t *testing.T, fields map[string]events.TelemetryValue, name, want string) {
	t.Helper()
	v, ok := fields[name]
	if want == "" {
		if ok {
			t.Errorf("field %s present (%v), want absent", name, v.StringVal)
		}
		return
	}
	if !ok {
		t.Fatalf("field %s absent, want %q", name, want)
	}
	if v.StringVal == nil {
		t.Fatalf("field %s has nil StringVal, want %q", name, want)
	}
	if *v.StringVal != want {
		t.Errorf("field %s = %q, want %q", name, *v.StringVal, want)
	}
}
