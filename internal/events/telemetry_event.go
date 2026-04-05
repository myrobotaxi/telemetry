package events

import "time"

// VehicleTelemetryEvent represents a batch of telemetry fields received
// from a single vehicle. Published by the telemetry receiver whenever a
// protobuf payload is decoded.
type VehicleTelemetryEvent struct {
	BasePayload
	VIN       string
	CreatedAt time.Time
	Fields    map[string]TelemetryValue
}

// EventTopic returns TopicVehicleTelemetry.
func (VehicleTelemetryEvent) EventTopic() Topic { return TopicVehicleTelemetry }

// TelemetryValue holds a single telemetry field value. Exactly one of the
// pointer fields is non-nil, determined by the protobuf field type.
type TelemetryValue struct {
	StringVal   *string
	FloatVal    *float64
	IntVal      *int64
	BoolVal     *bool
	LocationVal *Location
	Invalid     bool // True when the vehicle marks the datum as invalid
}

// Location is a geographic coordinate pair.
type Location struct {
	Latitude  float64
	Longitude float64
}
