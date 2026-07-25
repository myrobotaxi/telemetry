package ws

import "github.com/myrobotaxi/telemetry/internal/events"

// splitDoorStateField unpacks the DoorState bitmask (Tesla proto field 58,
// packed by internal/telemetry/converters_doors.go using the shared
// events.Door* bit layout) into the two boolean wire fields MYR-252
// contracts: frunkOpen (Doors.TrunkFront) and trunkOpen (Doors.TrunkRear).
// The other four door bits are decoded upstream but intentionally not
// surfaced — the owner cabin controls only read back frunk/trunk. A non-int
// value is ignored (defensive: the decoder always emits IntVal for
// doorState).
func splitDoorStateField(out map[string]any, val events.TelemetryValue) {
	if val.IntVal == nil {
		return
	}
	bits := *val.IntVal
	out["frunkOpen"] = events.DoorOpen(bits, events.DoorFrunk)
	out["trunkOpen"] = events.DoorOpen(bits, events.DoorTrunk)
}
