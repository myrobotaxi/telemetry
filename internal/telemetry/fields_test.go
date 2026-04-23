package telemetry

import (
	"testing"

	tpb "github.com/tnando/my-robo-taxi-telemetry/internal/telemetry/proto/tesla"
)

func TestFieldMap_ChargeStateSourcedFromProto179(t *testing.T) {
	t.Parallel()

	// MYR-42 (2026-04-23): chargeState sources from proto 179
	// DetailedChargeState, NOT proto 2 ChargeState. Tesla firmware ≥ 2024.44.25
	// accepts proto 2 in fleet_telemetry_config but never emits it; proto 179
	// fires on the same transitions with identical enum string values.
	got, ok := fieldMap[tpb.Field_DetailedChargeState]
	if !ok {
		t.Fatal("fieldMap missing entry for Field_DetailedChargeState (proto 179)")
	}
	if got != FieldChargeState {
		t.Errorf("fieldMap[Field_DetailedChargeState] = %q, want %q (the chargeState wire field)", got, FieldChargeState)
	}
	if FieldChargeState != "chargeState" {
		t.Errorf("FieldChargeState internal name = %q, want %q (contract wire name)", FieldChargeState, "chargeState")
	}
}

func TestFieldMap_ChargeStateProto2HeldOut(t *testing.T) {
	t.Parallel()

	// MYR-42 §10 DV-19: proto 2 ChargeState is deprecated on recent Tesla
	// firmware and MUST NOT be in fieldMap. If a future firmware re-
	// populates it this test will fail and the decision should be
	// reconsidered.
	if _, ok := fieldMap[tpb.Field_ChargeState]; ok {
		t.Error("fieldMap must not include Field_ChargeState (proto 2) — Tesla firmware ≥ 2024.44.25 does not populate it; source from Field_DetailedChargeState (proto 179) instead; see MYR-42 and websocket-protocol.md §10 DV-19")
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
