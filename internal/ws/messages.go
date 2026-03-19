// Package ws provides a WebSocket server for browser clients. It manages
// client connections, authentication, heartbeat, and broadcasts vehicle
// telemetry updates to authorized users.
package ws

import "encoding/json"

// Message type constants matching the frontend protocol.
const (
	msgTypeAuth          = "auth"
	msgTypeVehicleUpdate = "vehicle_update"
	msgTypeDriveStarted  = "drive_started"
	msgTypeDriveEnded    = "drive_ended"
	msgTypeConnectivity  = "connectivity"
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

// driveStartedPayload is the server-to-client payload sent when the drive
// detector identifies a new drive.
type driveStartedPayload struct {
	VehicleID string  `json:"vehicleId"`
	DriveID   string  `json:"driveId"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
	Timestamp string  `json:"timestamp"`
}

// driveEndedPayload is the server-to-client payload sent when a drive
// completes. Contains summary statistics for the trip.
type driveEndedPayload struct {
	VehicleID string  `json:"vehicleId"`
	DriveID   string  `json:"driveId"`
	Distance  float64 `json:"distance"`
	Duration  string  `json:"duration"`
	AvgSpeed  float64 `json:"avgSpeed"`
	MaxSpeed  float64 `json:"maxSpeed"`
	Timestamp string  `json:"timestamp"`
}

// connectivityPayload is the server-to-client payload sent when a vehicle
// connects or disconnects from the telemetry server.
type connectivityPayload struct {
	VehicleID string `json:"vehicleId"`
	Status    string `json:"status"`
	Timestamp string `json:"timestamp"`
}

// Error code constants returned to clients.
const (
	errCodeAuthFailed  = "auth_failed"
	errCodeAuthTimeout = "auth_timeout"
)
