package telemetry

import (
	"testing"

	tpb "github.com/tnando/my-robo-taxi-telemetry/internal/telemetry/proto/tesla"
)

func TestFieldMap_ChargeStateConstants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		protoEnum tpb.Field
		wantField FieldName
	}{
		{
			name:      "ChargeState proto 2 maps to FieldChargeState",
			protoEnum: tpb.Field_ChargeState,
			wantField: FieldChargeState,
		},
		{
			name:      "DetailedChargeState proto 179 maps to FieldDetailedChargeState",
			protoEnum: tpb.Field_DetailedChargeState,
			wantField: FieldDetailedChargeState,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := fieldMap[tt.protoEnum]
			if !ok {
				t.Fatalf("fieldMap[%v] not found; expected %q", tt.protoEnum, tt.wantField)
			}
			if got != tt.wantField {
				t.Errorf("fieldMap[%v] = %q, want %q", tt.protoEnum, got, tt.wantField)
			}
		})
	}
}

func TestFieldMap_ChargeStateAndDetailedChargeStateAreDistinct(t *testing.T) {
	t.Parallel()

	if FieldChargeState == FieldDetailedChargeState {
		t.Fatal("FieldChargeState and FieldDetailedChargeState must have different values")
	}

	chargeField, ok := fieldMap[tpb.Field_ChargeState]
	if !ok {
		t.Fatal("fieldMap missing entry for Field_ChargeState (proto 2)")
	}

	detailedField, ok := fieldMap[tpb.Field_DetailedChargeState]
	if !ok {
		t.Fatal("fieldMap missing entry for Field_DetailedChargeState (proto 179)")
	}

	if chargeField == detailedField {
		t.Errorf("fieldMap entries for ChargeState and DetailedChargeState must differ; both are %q", chargeField)
	}
}

func TestFieldMap_TimeToFullCharge(t *testing.T) {
	t.Parallel()

	got, ok := fieldMap[tpb.Field_TimeToFullCharge]
	if !ok {
		t.Fatal("fieldMap missing entry for Field_TimeToFullCharge (proto 43)")
	}
	if got != FieldTimeToFull {
		t.Errorf("fieldMap[Field_TimeToFullCharge] = %q, want %q", got, FieldTimeToFull)
	}
	if FieldTimeToFull != "timeToFull" {
		t.Errorf("FieldTimeToFull internal name = %q, want %q (contract wire name)", FieldTimeToFull, "timeToFull")
	}
}

func TestFieldMap_EstimatedHoursToChargeTerminationHeldOut(t *testing.T) {
	t.Parallel()

	// MYR-28 §7.1 flip condition: proto 190 stays out of fieldMap until the
	// Trip Planner Supercharger capture (MYR-25) confirms MYR-28's decision
	// to source timeToFull from proto 43 does NOT flip to proto 190.
	if _, ok := fieldMap[tpb.Field_EstimatedHoursToChargeTermination]; ok {
		t.Error("fieldMap must not include Field_EstimatedHoursToChargeTermination (proto 190) until MYR-25 closes; see MYR-28 §7.1 flip condition")
	}
}
