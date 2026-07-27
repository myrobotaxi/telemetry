package telemetry

import (
	"encoding/json"
	"testing"
)

// TestVehicleConfigDecodesHasSeatCooling covers the MYR-308 JSON decode. The
// three cases are semantically distinct and MUST stay so:
//
//	true    → the car has ventilated seats (the client's car returns this)
//	false   → AUTHORITATIVE no; clients must stop offering seat-cooling controls
//	absent  → unknown; clients fall back to the seatCooler*-presence heuristic
//
// Collapsing absent into false is the failure this test exists to prevent — it
// would tell every owner on older firmware that their cooled seats do not exist.
func TestVehicleConfigDecodesHasSeatCooling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		body     string
		wantNil  bool
		wantBool bool
	}{
		{
			name:     "true alongside trim_badging",
			body:     `{"response":{"vehicle_config":{"trim_badging":"p100d","has_seat_cooling":true}}}`,
			wantBool: true,
		},
		{
			name:     "explicit false is authoritative, not absent",
			body:     `{"response":{"vehicle_config":{"trim_badging":"p100d","has_seat_cooling":false}}}`,
			wantBool: false,
		},
		{
			name:    "absent key decodes to nil, NOT false",
			body:    `{"response":{"vehicle_config":{"trim_badging":"p100d"}}}`,
			wantNil: true,
		},
		{
			name:    "absent vehicle_config object entirely",
			body:    `{"response":{"vehicle_state":{"locked":true}}}`,
			wantNil: true,
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
			if tt.wantNil {
				if vc != nil && vc.HasSeatCooling != nil {
					t.Fatalf("has_seat_cooling = %v, want nil (absent must never mean false)",
						*vc.HasSeatCooling)
				}
				return
			}
			if vc == nil || vc.HasSeatCooling == nil {
				t.Fatal("has_seat_cooling did not decode")
			}
			if *vc.HasSeatCooling != tt.wantBool {
				t.Errorf("has_seat_cooling = %v, want %v", *vc.HasSeatCooling, tt.wantBool)
			}
		})
	}
}

// TestVehicleDataToFieldsSeatCoolingCapable covers the REST→internal-field
// mapping, mirroring TestVehicleDataToFields_VersionAndTrim for MYR-279.
//
// Note the asymmetry with trim, which is deliberate: a blank trim string is
// skipped (it would overwrite a known badge with ""), but a FALSE capability is
// KEPT, because false is the authoritative answer and is the entire point of the
// field. Only an absent key is skipped.
func TestVehicleDataToFieldsSeatCoolingCapable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		hasCooling  *bool
		wantPresent bool
		wantValue   bool
	}{
		{name: "true is mapped", hasCooling: boolPtrTest(true), wantPresent: true, wantValue: true},
		{name: "false is mapped, not skipped", hasCooling: boolPtrTest(false), wantPresent: true, wantValue: false},
		{name: "absent is skipped so the column stays NULL", hasCooling: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data := &VehicleData{
				VehicleConfig: &VehicleDataVehicleConfig{HasSeatCooling: tt.hasCooling},
			}
			fields := vehicleDataToFields(data)
			v, ok := fields[string(FieldSeatCoolingCapable)]
			if !tt.wantPresent {
				if ok {
					t.Fatal("absent has_seat_cooling must not produce a field — the column must stay NULL " +
						"so clients read it as unknown, not as 'no seat cooling'")
				}
				return
			}
			if !ok {
				t.Fatalf("seatCoolingCapable missing from mapped fields: %+v", fields)
			}
			if v.BoolVal == nil || *v.BoolVal != tt.wantValue {
				t.Errorf("seatCoolingCapable = %+v, want %v", v.BoolVal, tt.wantValue)
			}
		})
	}
}

func boolPtrTest(b bool) *bool { return &b }
