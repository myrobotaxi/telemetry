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

	// Client->server control frames added by MYR-46 (DV-07). The contract
	// catalog lives in websocket-protocol.md §5; payload shapes are
	// canonical in schemas/ws-messages.schema.json (Subscribe-, Unsubscribe-,
	// PingPayload). The schema uses singular `vehicleId`, not `vehicleIds[]`.
	msgTypeSubscribe   = "subscribe"
	msgTypeUnsubscribe = "unsubscribe"
	msgTypePing        = "ping"
	msgTypePong        = "pong"
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

// subscribePayload is the client-to-server request to (re)assert
// streaming for a specific vehicle. Schema: SubscribePayload in
// schemas/ws-messages.schema.json. SinceSeq is reserved for DV-02 and
// is currently parsed but ignored.
type subscribePayload struct {
	VehicleID string `json:"vehicleId"`
	SinceSeq  *int64 `json:"sinceSeq,omitempty"`
}

// unsubscribePayload is the client-to-server request to stop streaming
// updates for a specific vehicle without closing the WebSocket.
type unsubscribePayload struct {
	VehicleID string `json:"vehicleId"`
}

// pingPayload is the client-to-server application-level liveness probe.
// The server responds with msgTypePong echoing the same Nonce. Today
// this is reserved for Apple-platform consumers (NFR-3.36 / NFR-3.36a-d);
// browser/Node consumers rely on transport-level RFC 6455 PING/PONG.
type pingPayload struct {
	Nonce string `json:"nonce,omitempty"`
}

// pongPayload is the server-to-client response to a client-initiated
// ping. Echoes the nonce so the client can compute round-trip latency.
type pongPayload struct {
	Nonce string `json:"nonce,omitempty"`
}

