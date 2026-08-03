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

// A `vehicle_not_owned` subscribe no longer closes the connection, so the
// 4002 close code this file used to declare is gone from here. The only
// remaining producer of 4002 is the hub's revocation path
// (closeCodeVehicleAccessRevoked in hub.go), which closes the whole session
// because access itself ended rather than because one frame was refused.
// See websocket-protocol.md §6.1.1 / §6.2 and MYR-373.

// handleClientFrame parses one client->server frame and dispatches it
// to the appropriate handler. Returns false when the connection MUST
// close after the dispatch (today only a session whose access was revoked
// mid-connection); the caller (readPump) exits in that case. Returns true for
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

	// A session whose access was revoked mid-connection is already being torn
	// down; it must not be able to talk its way back into the stream on the
	// way out (MYR-373). This check comes BEFORE owns() precisely because
	// owns() would say yes: it reads the handshake-frozen vehicleIDs, which
	// still contains the vehicle the owner just took away. Returning false
	// exits the readPump; no error frame and no close are needed because
	// RevokeUserAccess has already closed this connection with 4002.
	if c.revoked.Load() {
		return false
	}

	if !c.owns(p.VehicleID) {
		// Typed `vehicle_not_owned` error frame, and the connection STAYS
		// OPEN (behavior change, MYR-373 — see websocket-protocol.md §6.1.1).
		//
		// This used to close with 4002, which became actively harmful the
		// moment revocation started closing sockets: a viewer who loses a
		// grant reconnects, re-handshakes into the correctly reduced set,
		// re-sends the `subscribe` its local state still lists, and gets
		// closed again — a reconnect loop the SERVER drives, on a connection
		// that is otherwise perfectly valid and may hold other vehicles the
		// caller legitimately owns. Refusing one subscription is not grounds
		// for destroying the session; the typed error already tells the
		// client exactly what happened, which is what the SDK acts on.
		// (The client half — dropping the stale vehicle from its subscription
		// list — is MYR-432.)
		errCtx, cancel := context.WithTimeout(ctx, writeTimeout)
		defer cancel()
		_ = sendError(errCtx, c.conn, wserrors.ErrCodeVehicleNotOwned,
			"vehicle is not in the caller's ownership set", writeTimeout)
		c.logger.Warn("subscribe: vehicle_not_owned",
			slog.String("user_id", c.userID),
			slog.String("vehicle_id", p.VehicleID),
		)
		return true
	}

	c.subscribe(p.VehicleID)
	c.logger.Debug("subscribe: ok",
		slog.String("user_id", c.userID),
		slog.String("vehicle_id", p.VehicleID),
	)

	// Unicast the persisted snapshot for this vehicle so the client sees
	// model/year/color/estimatedRange/fsdMilesSinceReset immediately,
	// rather than waiting on the next live Tesla frame (MYR-137).
	c.hub.sendSnapshot(ctx, c, p.VehicleID, writeTimeout)
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
