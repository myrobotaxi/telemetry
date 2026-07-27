package events

import "time"

// TelemetrySource identifies where a VehicleTelemetryEvent's values came from.
// Two publishers exist: the live Tesla stream decoder and the MYR-260 REST
// /vehicle_data backfill for non-streaming cars.
type TelemetrySource string

const (
	// SourceStream is the live Tesla fleet-telemetry mTLS stream. It is the
	// ZERO VALUE, so any frame that does not explicitly say otherwise is
	// treated as streamed.
	SourceStream TelemetrySource = ""

	// SourceRESTBackfill marks the synthetic frame the ServiceStatusMonitor
	// publishes from a REST /vehicle_data read (MYR-260). Consumers that
	// reason about stream liveness — the MYR-300 freshness gate — MUST NOT
	// count one of these as evidence that the car is streaming, or the gate
	// would latch on its own output.
	SourceRESTBackfill TelemetrySource = "rest_backfill"
)

// VehicleTelemetryEvent represents a batch of telemetry fields received
// from a single vehicle. Published by the telemetry receiver whenever a
// protobuf payload is decoded.
type VehicleTelemetryEvent struct {
	BasePayload
	VIN       string
	CreatedAt time.Time
	Fields    map[string]TelemetryValue
	// Source is the provenance of this frame. Empty (SourceStream) means the
	// live stream — the historical and overwhelmingly common case.
	Source TelemetrySource
}

// EventTopic returns TopicVehicleTelemetry.
func (VehicleTelemetryEvent) EventTopic() Topic { return TopicVehicleTelemetry }

// Streamed reports whether this frame came from the live vehicle stream (as
// opposed to a REST backfill). Used by the MYR-300 stream-freshness gate.
func (e VehicleTelemetryEvent) Streamed() bool { return e.Source != SourceRESTBackfill }

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
