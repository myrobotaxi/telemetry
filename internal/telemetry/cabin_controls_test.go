package telemetry

import (
	"testing"

	tpb "github.com/myrobotaxi/telemetry/internal/telemetry/proto/tesla"
)

func hvacAutoModeVal(m tpb.HvacAutoModeState) *tpb.Value {
	return &tpb.Value{Value: &tpb.Value_HvacAutoModeValue{HvacAutoModeValue: m}}
}

func mediaStatusVal(s tpb.MediaStatus) *tpb.Value {
	return &tpb.Value{Value: &tpb.Value_MediaStatusValue{MediaStatusValue: s}}
}

// TestConvertCabinControls_Enums covers the two Group B enum decodes
// (MYR-252): HvacAutoMode (proto 197) and MediaPlaybackStatus (proto 242).
// The resulting strings MUST match the vehicle-state.schema.json enums.
func TestConvertCabinControls_Enums(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		field tpb.Field
		in    *tpb.Value
		want  string
	}{
		{"hvacAutoMode On", tpb.Field_HvacAutoMode, hvacAutoModeVal(tpb.HvacAutoModeState_HvacAutoModeStateOn), "On"},
		{"hvacAutoMode Override", tpb.Field_HvacAutoMode, hvacAutoModeVal(tpb.HvacAutoModeState_HvacAutoModeStateOverride), "Override"},
		{"hvacAutoMode Unknown", tpb.Field_HvacAutoMode, hvacAutoModeVal(tpb.HvacAutoModeState_HvacAutoModeStateUnknown), "Unknown"},
		{"hvacAutoMode string fallback", tpb.Field_HvacAutoMode, stringVal("On"), "On"},
		{"media Playing", tpb.Field_MediaPlaybackStatus, mediaStatusVal(tpb.MediaStatus_MediaStatusPlaying), "Playing"},
		{"media Paused", tpb.Field_MediaPlaybackStatus, mediaStatusVal(tpb.MediaStatus_MediaStatusPaused), "Paused"},
		{"media Stopped", tpb.Field_MediaPlaybackStatus, mediaStatusVal(tpb.MediaStatus_MediaStatusStopped), "Stopped"},
		{"media Unknown", tpb.Field_MediaPlaybackStatus, mediaStatusVal(tpb.MediaStatus_MediaStatusUnknown), "Unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := convertValue(tt.field, tt.in)
			if err != nil {
				t.Fatalf("convertValue error: %v", err)
			}
			if got.StringVal == nil {
				t.Fatalf("StringVal = nil, want %q", tt.want)
			}
			if *got.StringVal != tt.want {
				t.Errorf("= %q, want %q", *got.StringVal, tt.want)
			}
		})
	}
}

// TestConvertCabinControls_Bools covers the Group B boolean decodes
// (MYR-252): HvacACEnabled (196), SeatVentEnabled (254), ChargePortDoorOpen
// (183). All route through convertBool.
func TestConvertCabinControls_Bools(t *testing.T) {
	t.Parallel()

	fields := []tpb.Field{
		tpb.Field_HvacACEnabled,
		tpb.Field_SeatVentEnabled,
		tpb.Field_ChargePortDoorOpen,
	}
	for _, f := range fields {
		for _, want := range []bool{true, false} {
			f, want := f, want
			t.Run(f.String(), func(t *testing.T) {
				t.Parallel()
				got, err := convertValue(f, boolVal(want))
				if err != nil {
					t.Fatalf("convertValue error: %v", err)
				}
				if got.BoolVal == nil || *got.BoolVal != want {
					t.Errorf("BoolVal = %v, want %v", got.BoolVal, want)
				}
			})
		}
	}
}

// TestFieldMap_CabinControlsCoverage asserts every MYR-252 Group A+B proto
// field maps to the expected internal (== wire) name. Guards against a proto
// field number or internal-name typo silently dropping a contracted field.
func TestFieldMap_CabinControlsCoverage(t *testing.T) {
	t.Parallel()

	want := map[tpb.Field]FieldName{
		// Group A (already streaming; contracted by MYR-252).
		tpb.Field_HvacPower:                   FieldHvacPower,
		tpb.Field_HvacFanSpeed:                FieldHvacFanSpeed,
		tpb.Field_HvacLeftTemperatureRequest:  FieldDriverTempSetting,
		tpb.Field_HvacRightTemperatureRequest: FieldPassengerTempSetting,
		tpb.Field_Locked:                      FieldLocked,
		tpb.Field_SeatHeaterLeft:              FieldSeatHeaterLeft,
		tpb.Field_SeatHeaterRight:             FieldSeatHeaterRight,
		// Group B (newly added to fieldMap + fleet config).
		tpb.Field_HvacAutoMode:                 FieldHvacAutoMode,
		tpb.Field_HvacACEnabled:                FieldHvacACEnabled,
		tpb.Field_SeatHeaterRearLeft:           FieldSeatHeaterRearLeft,
		tpb.Field_SeatHeaterRearCenter:         FieldSeatHeaterRearCenter,
		tpb.Field_SeatHeaterRearRight:          FieldSeatHeaterRearRight,
		tpb.Field_ClimateSeatCoolingFrontLeft:  FieldSeatCoolerLeft,
		tpb.Field_ClimateSeatCoolingFrontRight: FieldSeatCoolerRight,
		tpb.Field_SeatVentEnabled:              FieldSeatVentEnabled,
		tpb.Field_ChargePortDoorOpen:           FieldChargePortDoorOpen,
		tpb.Field_DoorState:                    FieldDoorState,
		tpb.Field_MediaPlaybackStatus:          FieldMediaPlaybackStatus,
		tpb.Field_MediaAudioVolume:             FieldMediaVolume,
	}

	for proto, wantName := range want {
		got, ok := fieldMap[proto]
		if !ok {
			t.Errorf("fieldMap missing %s (proto %d)", proto.String(), proto)
			continue
		}
		if got != wantName {
			t.Errorf("fieldMap[%s] = %q, want %q", proto.String(), got, wantName)
		}
	}
}
