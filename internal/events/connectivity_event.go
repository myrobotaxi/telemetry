package events

import "time"

// ConnectivityStatus indicates whether a vehicle is connected to the
// telemetry server.
type ConnectivityStatus int

const (
	// StatusConnected indicates the vehicle has an active mTLS WebSocket
	// connection to the telemetry server.
	StatusConnected ConnectivityStatus = iota

	// StatusDisconnected indicates the vehicle's connection was closed
	// or lost.
	StatusDisconnected
)

// String returns a human-readable representation of the connectivity status.
func (s ConnectivityStatus) String() string {
	switch s {
	case StatusConnected:
		return "connected"
	case StatusDisconnected:
		return "disconnected"
	default:
		return "unknown"
	}
}

// ConnectivityEvent is published when a vehicle connects to or disconnects
// from the telemetry server.
type ConnectivityEvent struct {
	BasePayload
	VIN       string
	Status    ConnectivityStatus
	Timestamp time.Time
}

// EventTopic returns TopicConnectivity.
func (ConnectivityEvent) EventTopic() Topic { return TopicConnectivity }
