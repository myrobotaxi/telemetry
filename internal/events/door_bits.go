package events

// DoorBit is a single door position within the packed DoorState bitmask the
// telemetry decoder produces for Tesla proto field 58 (DoorState → the
// `Doors` protobuf message, MYR-252). The decoder folds each open door into
// one bit (see internal/telemetry/converters_doors.go); internal/ws unpacks
// the bits it surfaces on the wire (frunkOpen, trunkOpen — see
// internal/ws/door_fields.go). The layout lives here, in the package both
// the pack and unpack ends already import, so the two cannot drift.
type DoorBit int64

const (
	DoorDriverFront    DoorBit = 1 << 0
	DoorDriverRear     DoorBit = 1 << 1
	DoorPassengerFront DoorBit = 1 << 2
	DoorPassengerRear  DoorBit = 1 << 3
	DoorFrunk          DoorBit = 1 << 4 // Doors.TrunkFront
	DoorTrunk          DoorBit = 1 << 5 // Doors.TrunkRear
)

// DoorOpen reports whether the given door bit is set in a packed DoorState
// bitmask (as carried by TelemetryValue.IntVal).
func DoorOpen(bitmask int64, b DoorBit) bool {
	return bitmask&int64(b) != 0
}
