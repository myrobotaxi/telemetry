// Package ws provides a WebSocket server for browser clients. It manages
// client connections, authentication, heartbeat, and broadcasts vehicle
// telemetry updates to authorized users.
package ws

import (
	"encoding/json"

	"github.com/tnando/my-robo-taxi-telemetry/internal/wserrors"
)

// Message type constants matching the frontend protocol.
const (
	msgTypeAuth          = "auth"
	msgTypeAuthOk        = "auth_ok"
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

// authOkPayload is the server-to-client positive authentication
// acknowledgement. Emitted as the FIRST frame after successful
// Authenticator.ValidateToken + GetUserVehicles + Hub.Register.
// Triggers the SDK connectionState transition connecting -> connected (C-3).
// See websocket-protocol.md §2.3 for the full handshake contract.
type authOkPayload struct {
	UserID       string `json:"userId"`
	VehicleCount int    `json:"vehicleCount"`
	IssuedAt     string `json:"issuedAt"`
}

// errorPayload is the server-to-client payload for error messages.
// Code is the typed enum from wserrors so the compiler refuses string
// literals at every error-frame construction site.
type errorPayload struct {
	Code    wserrors.ErrorCode `json:"code"`
	Message string             `json:"message"`
}

// driveStartedPayload is the server-to-client payload sent when the drive
// detector identifies a new drive.
type driveStartedPayload struct {
	VehicleID     string        `json:"vehicleId"`
	DriveID       string        `json:"driveId"`
	StartLocation startLocation `json:"startLocation"`
	Timestamp     string        `json:"timestamp"`
}

type startLocation struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// driveEndedPayload is the server-to-client payload sent when a drive
// completes. Contains summary statistics for the trip.
// DurationSeconds is seconds as float64, matching the v1 wire contract
// (websocket-protocol.md §4.3, DV-12).
type driveEndedPayload struct {
	VehicleID       string  `json:"vehicleId"`
	DriveID         string  `json:"driveId"`
	Distance        float64 `json:"distance"`
	DurationSeconds float64 `json:"durationSeconds"`
	AvgSpeed        float64 `json:"avgSpeed"`
	MaxSpeed        float64 `json:"maxSpeed"`
	Timestamp       string  `json:"timestamp"`
}

// connectivityPayload is the server-to-client payload sent when a vehicle
// connects or disconnects from the telemetry server.
type connectivityPayload struct {
	VehicleID string `json:"vehicleId"`
	Online    bool   `json:"online"`
	Timestamp string `json:"timestamp"`
}

