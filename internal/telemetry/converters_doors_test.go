package telemetry

import (
	"testing"

	"github.com/myrobotaxi/telemetry/internal/events"
	tpb "github.com/myrobotaxi/telemetry/internal/telemetry/proto/tesla"
)

// doorsVal builds a DoorState Value carrying the Doors oneof variant
// (proto field 58, door_value tag 21).
func doorsVal(driverFront, driverRear, passFront, passRear, trunkFront, trunkRear bool) *tpb.Value {
	return &tpb.Value{Value: &tpb.Value_DoorValue{DoorValue: &tpb.Doors{
		DriverFront:    driverFront,
		DriverRear:     driverRear,
		PassengerFront: passFront,
		PassengerRear:  passRear,
		TrunkFront:     trunkFront,
		TrunkRear:      trunkRear,
	}}}
}

// TestConvertDoorState_BitDecode is the required DoorState decode test
// (MYR-252). Tesla emits DoorState as a `Doors` struct of six booleans; the
// decoder folds them into the events.Door* bitmask carried as IntVal. The
// ws layer later unpacks frunk (TrunkFront) and trunk (TrunkRear).
func TestConvertDoorState_BitDecode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   *tpb.Value
		want int64
	}{
		{"all closed", doorsVal(false, false, false, false, false, false), 0},
		{
			"frunk only (TrunkFront)",
			doorsVal(false, false, false, false, true, false),
			int64(events.DoorFrunk),
		},
		{
			"trunk only (TrunkRear)",
			doorsVal(false, false, false, false, false, true),
			int64(events.DoorTrunk),
		},
		{
			"frunk + trunk",
			doorsVal(false, false, false, false, true, true),
			int64(events.DoorFrunk) | int64(events.DoorTrunk),
		},
		{
			"driver front only",
			doorsVal(true, false, false, false, false, false),
			int64(events.DoorDriverFront),
		},
		{
			"all open",
			doorsVal(true, true, true, true, true, true),
			int64(events.DoorDriverFront) | int64(events.DoorDriverRear) |
				int64(events.DoorPassengerFront) | int64(events.DoorPassengerRear) |
				int64(events.DoorFrunk) | int64(events.DoorTrunk),
		},
		// Firmware fallback: aggregate delivered as a plain int passes
		// through unchanged so the same bit positions apply.
		{"int passthrough", intVal(int32(events.DoorTrunk)), int64(events.DoorTrunk)},
		{"long passthrough", longVal(int64(events.DoorFrunk)), int64(events.DoorFrunk)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := convertValue(tpb.Field_DoorState, tt.in)
			if err != nil {
				t.Fatalf("convertValue(DoorState) error: %v", err)
			}
			if got.IntVal == nil {
				t.Fatalf("convertValue(DoorState) IntVal = nil, want %d", tt.want)
			}
			if *got.IntVal != tt.want {
				t.Errorf("bitmask = %d, want %d", *got.IntVal, tt.want)
			}
			// Cross-check the accessor the ws layer uses.
			if tt.name == "frunk + trunk" {
				if !events.DoorOpen(*got.IntVal, events.DoorFrunk) {
					t.Error("DoorOpen(frunk) = false, want true")
				}
				if !events.DoorOpen(*got.IntVal, events.DoorTrunk) {
					t.Error("DoorOpen(trunk) = false, want true")
				}
			}
		})
	}
}

func TestConvertDoorState_WrongType(t *testing.T) {
	t.Parallel()
	if _, err := convertValue(tpb.Field_DoorState, stringVal("nope")); err == nil {
		t.Error("convertValue(DoorState, string) expected error, got nil")
	}
}
