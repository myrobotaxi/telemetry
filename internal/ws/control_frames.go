package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/coder/websocket"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// closeCodePermissionRevoked is the WebSocket close code emitted after
// a permission_denied / vehicle_not_owned error frame, per
// websocket-protocol.md §6.2 (DV-07 target). 4002 lives in the RFC 6455
// 4000-4999 application-specific range.
const closeCodePermissionRevoked websocket.StatusCode = 4002

// handleClientFrame parses one client->server frame and dispatches it
// to the appropriate handler. Returns false when the connection MUST
// close after the dispatch (today only the vehicle_not_owned subscribe
// path); the caller (readPump) exits in that case. Returns true for
// every other outcome — including parse errors and unknown frame
// types, which are logged-and-ignored so an out-of-spec frame from a
// future SDK does not poison an otherwise-healthy connection.
func (c *Client) handleClientFrame(ctx context.Context, data []byte, writeTimeout time.Duration) bool {
	var msg wsMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		c.logger.Debug("client frame: malformed JSON, ignoring",
			slog.String("user_id", c.userID),
			slog.Any("error", err),
		)
		return true
	}

	switch msg.Type {
	case msgTypeSubscribe:
		return c.handleSubscribeFrame(ctx, msg.Payload, writeTimeout)
	case msgTypeUnsubscribe:
		c.handleUnsubscribeFrame(msg.Payload)
		return true
	case msgTypePing:
		c.handlePingFrame(ctx, msg.Payload, writeTimeout)
		return true
	default:
		// Unknown frame type — silently ignored to preserve
		// forward-compatibility (websocket-protocol.md §5).
		return true
	}
}

// handleSubscribeFrame validates ownership and adds the vehicle to the
// active subscription set. On a non-owned vehicle, emits the typed
// error frame and closes the connection with code 4002. Returns false
// only on the close path.
func (c *Client) handleSubscribeFrame(ctx context.Context, raw json.RawMessage, writeTimeout time.Duration) bool {
	var p subscribePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		c.logger.Debug("subscribe: malformed payload, ignoring",
			slog.String("user_id", c.userID),
			slog.Any("error", err),
		)
		return true
	}
	if p.VehicleID == "" {
		c.logger.Debug("subscribe: empty vehicleId, ignoring",
			slog.String("user_id", c.userID),
		)
		return true
	}

	if !c.owns(p.VehicleID) {
		// Per websocket-protocol.md §6.1.1 + §6.2 (DV-07 target):
		// emit the typed error frame, then close with code 4002.
		errCtx, cancel := context.WithTimeout(ctx, writeTimeout)
		defer cancel()
		_ = sendError(errCtx, c.conn, wserrors.ErrCodeVehicleNotOwned,
			"vehicle is not in the caller's ownership set", writeTimeout)
		_ = c.conn.Close(closeCodePermissionRevoked, "vehicle_not_owned")
		c.logger.Warn("subscribe: vehicle_not_owned",
			slog.String("user_id", c.userID),
			slog.String("vehicle_id", p.VehicleID),
		)
		return false
	}

	c.subscribe(p.VehicleID)
	c.logger.Debug("subscribe: ok",
		slog.String("user_id", c.userID),
		slog.String("vehicle_id", p.VehicleID),
	)
	return true
}

// handleUnsubscribeFrame removes the vehicle from the active
// subscription set. Idempotent and never closes the connection.
func (c *Client) handleUnsubscribeFrame(raw json.RawMessage) {
	var p unsubscribePayload
	if err := json.Unmarshal(raw, &p); err != nil {
		c.logger.Debug("unsubscribe: malformed payload, ignoring",
			slog.String("user_id", c.userID),
			slog.Any("error", err),
		)
		return
	}
	if p.VehicleID == "" {
		return
	}
	c.unsubscribe(p.VehicleID)
	c.logger.Debug("unsubscribe: ok",
		slog.String("user_id", c.userID),
		slog.String("vehicle_id", p.VehicleID),
	)
}

// handlePingFrame echoes the client's nonce in a pong frame. Failure
// to write is logged at debug level and otherwise non-fatal — the
// transport-level liveness signal is still healthy because we just
// successfully read a frame.
func (c *Client) handlePingFrame(ctx context.Context, raw json.RawMessage, writeTimeout time.Duration) {
	var p pingPayload
	// Tolerate empty payload — the schema marks nonce optional.
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &p)
	}
	if err := writePong(ctx, c.conn, p.Nonce, writeTimeout); err != nil {
		c.logger.Debug("pong write failed",
			slog.String("user_id", c.userID),
			slog.Any("error", err),
		)
	}
}

// writePong writes a server->client pong echoing the nonce.
func writePong(ctx context.Context, conn *websocket.Conn, nonce string, writeTimeout time.Duration) error {
	writeCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()

	payload, err := json.Marshal(pongPayload{Nonce: nonce})
	if err != nil {
		return fmt.Errorf("writePong: marshal payload: %w", err)
	}
	msg, err := json.Marshal(wsMessage{Type: msgTypePong, Payload: payload})
	if err != nil {
		return fmt.Errorf("writePong: marshal envelope: %w", err)
	}
	if err := conn.Write(writeCtx, websocket.MessageText, msg); err != nil {
		return fmt.Errorf("writePong: write: %w", err)
	}
	return nil
}
