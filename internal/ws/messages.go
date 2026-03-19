// Package ws provides a WebSocket server for browser clients. It manages
// client connections, authentication, heartbeat, and broadcasts vehicle
// telemetry updates to authorized users.
package ws

import "encoding/json"

// Message type constants matching the frontend protocol.
const (
	msgTypeAuth          = "auth"
	msgTypeVehicleUpdate = "vehicle_update"
	msgTypeHeartbeat     = "heartbeat"
	msgTypeError         = "error"
)

// wsMessage is the envelope for all WebSocket messages exchanged with
// browser clients.
type wsMessage struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// vehicleUpdatePayload is the server-to-client payload for real-time
// vehicle field updates. VehicleID is the database cuid, not the VIN.
type vehicleUpdatePayload struct {
	VehicleID string         `json:"vehicleId"`
	Fields    map[string]any `json:"fields"`
	Timestamp string         `json:"timestamp"`
}

// authPayload is the client-to-server payload sent immediately after
// WebSocket connection to authenticate the session.
type authPayload struct {
	Token string `json:"token"`
}

// errorPayload is the server-to-client payload for error messages.
type errorPayload struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error code constants returned to clients.
const (
	errCodeAuthFailed  = "auth_failed"
	errCodeAuthTimeout = "auth_timeout"
)
