package telemetry

import (
	"fmt"

	"github.com/myrobotaxi/telemetry/internal/events"
	tpb "github.com/myrobotaxi/telemetry/internal/telemetry/proto/tesla"
)

// convertDoorState decodes Tesla proto field 58 (DoorState). Tesla emits it
// via the `Doors` message (the door_value oneof variant, proto tag 21) — a
// struct of six booleans, NOT a scalar. Each open door is folded into one
// bit of an int64 bitmask whose layout is defined by the events.Door*
// constants; internal/ws later unpacks the two bits MYR-252 contracts on the
// wire: frunkOpen (Doors.TrunkFront) and trunkOpen (Doors.TrunkRear).
//
// Only the Doors-message variant is accepted. A scalar int/long is NOT
// treated as a pre-packed bitmask: our bit layout is a MyRoboTaxi convention,
// not Tesla's, so there is no attested int encoding to trust (MYR-42's rule:
// empirical capture before trusting an alternate encoding). An unexpected
// variant is an error, matching the string-fallback error path in the sibling
// enum converters — a malformed DoorState frame drops rather than silently
// mis-decoding door state.
func convertDoorState(v *tpb.Value) (events.TelemetryValue, error) {
	if dv, ok := v.Value.(*tpb.Value_DoorValue); ok {
		bits := packDoors(dv.DoorValue)
		return events.TelemetryValue{IntVal: &bits}, nil
	}
	return events.TelemetryValue{}, fmt.Errorf(
		"%w: DoorState expected doorValue, got %T", ErrUnexpectedValueType, v.Value,
	)
}

// packDoors folds the six Doors booleans into the events.Door* bitmask. A nil
// message (defensive: malformed frame) packs to 0 — every door closed.
func packDoors(d *tpb.Doors) int64 {
	if d == nil {
		return 0
	}
	var bits int64
	setBit := func(open bool, b events.DoorBit) {
		if open {
			bits |= int64(b)
		}
	}
	setBit(d.GetDriverFront(), events.DoorDriverFront)
	setBit(d.GetDriverRear(), events.DoorDriverRear)
	setBit(d.GetPassengerFront(), events.DoorPassengerFront)
	setBit(d.GetPassengerRear(), events.DoorPassengerRear)
	setBit(d.GetTrunkFront(), events.DoorFrunk)
	setBit(d.GetTrunkRear(), events.DoorTrunk)
	return bits
}
