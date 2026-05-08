package ws

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/tnando/my-robo-taxi-telemetry/internal/auth"
)

const (
	// sendBufSize is the capacity of the per-client send channel.
	// When the channel is full, the oldest message is dropped.
	sendBufSize = 64

	// readLimit is the maximum size of a client-to-server message.
	// Clients only send auth + keep-alive; 4 KiB is more than enough.
	readLimit = 4096
)

// Client represents a single authenticated WebSocket connection from a
// browser. Each client has its own send channel and read/write pumps.
type Client struct {
	conn       *websocket.Conn
	userID     string
	vehicleIDs []string // vehicles this user is authorized to see
	// allVehicles is the explicit "this client is authorized for every
	// vehicle" flag. It is set ONLY by the handshake when GetUserVehicles
	// returns the WildcardVehicleID sentinel (dev-mode NoopAuthenticator).
	// Production authenticators MUST NOT return that sentinel, so on
	// production this field stays false and an empty vehicleIDs slice
	// means deny-all per NFR-3.21.
	allVehicles bool
	// vehicleRoles maps vehicleID -> role for this client. Populated at
	// handshake time alongside vehicleIDs (handler.go authenticateClient).
	// Per websocket-protocol.md §4.6, the hub looks up the role here to
	// pick the role-appropriate pre-marshaled frame to enqueue. A
	// missing entry resolves to the empty Role("") sentinel and the
	// hub treats it as deny-all (fail-closed). See DV-09 for the
	// known mid-connection refresh gap (role downgrade requires
	// reconnect).
	vehicleRoles map[string]auth.Role
	// defaultRole is the fallback role consulted ONLY when allVehicles=true
	// and the per-vehicle vehicleRoles map has no entry for the requested
	// vehicleID. Set by the handshake to auth.RoleOwner for the dev-mode
	// NoopAuthenticator path (whose ResolveRole returns RoleOwner
	// unconditionally) so dev-mode clients receive role-projected frames
	// for every vehicle the server is broadcasting for, instead of the
	// empty Role("") deny-all sentinel that left them silently filtered
	// out (MYR-66). Production clients have allVehicles=false; defaultRole
	// is never consulted, so the fail-closed deny-all posture for clients
	// without an explicit vehicleRoles entry is preserved.
	defaultRole auth.Role
	// subscribed tracks which of the client's owned vehicles are
	// currently active subscriptions. Initialized at handshake from
	// vehicleIDs (so a client that never sends subscribe/unsubscribe
	// receives every owned vehicle, matching pre-MYR-46 behavior).
	// subscribe ADDS to the set after an ownership check; unsubscribe
	// REMOVES. Mutations are guarded by subMu, NOT by Hub.mu, so the
	// per-VIN broadcast hot-path (Hub.RLock) does not contend with the
	// readPump.
	subscribed map[string]struct{}
	subMu      sync.RWMutex
	remoteAddr string
	send       chan []byte
	hub        *Hub
	logger     *slog.Logger
}

// newClient creates a Client that is not yet authenticated. The userID and
// vehicleIDs are populated after the auth handshake completes.
func newClient(conn *websocket.Conn, hub *Hub, logger *slog.Logger) *Client {
	return &Client{
		conn:         conn,
		send:         make(chan []byte, sendBufSize),
		vehicleRoles: make(map[string]auth.Role),
		subscribed:   make(map[string]struct{}),
		hub:          hub,
		logger:       logger,
	}
}

// roleFor returns the role this client holds against vehicleID. Resolution
// order: (1) the per-vehicle vehicleRoles map populated at handshake; (2)
// for clients with allVehicles=true that lack a per-vehicle entry, the
// defaultRole set at handshake (the dev-mode NoopAuthenticator path);
// (3) the empty Role("") fail-closed sentinel, which the mask layer in
// internal/mask interprets as deny-all. Production clients (allVehicles=
// false) skip step 2, so a missing vehicleRoles entry stays deny-all.
func (c *Client) roleFor(vehicleID string) auth.Role {
	if c == nil {
		return auth.Role("")
	}
	if role, ok := c.vehicleRoles[vehicleID]; ok {
		return role
	}
	if c.allVehicles {
		return c.defaultRole
	}
	return auth.Role("")
}

// writePump reads messages from the send channel and writes them to the
// WebSocket connection. It exits when the send channel is closed or the
// context is cancelled.
func (c *Client) writePump(ctx context.Context, writeTimeout time.Duration) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-c.send:
			if !ok {
				// Hub closed the channel — send a close frame.
				_ = c.conn.Close(websocket.StatusGoingAway, "server shutting down")
				return
			}
			if err := c.writeMessage(ctx, msg, writeTimeout); err != nil {
				c.logger.Debug("write failed, closing client",
					slog.String("user_id", c.userID),
					slog.Any("error", err),
				)
				return
			}
			c.hub.metrics.IncMessagesSent()
		}
	}
}

// readPump reads messages from the WebSocket. After authentication,
// it dispatches client->server control frames (subscribe, unsubscribe,
// ping — DV-07) and ignores any other frame type so unknown messages
// from a future SDK do not poison the connection. Returns when the
// socket is closed or the context cancels.
func (c *Client) readPump(ctx context.Context, writeTimeout time.Duration) {
	c.conn.SetReadLimit(readLimit)
	for {
		_, data, err := c.conn.Read(ctx)
		if err != nil {
			if !isNormalClose(err) {
				c.logger.Debug("read error",
					slog.String("user_id", c.userID),
					slog.Any("error", err),
				)
			}
			return
		}
		if !c.handleClientFrame(ctx, data, writeTimeout) {
			// Returning false signals a hard close (subscribe to a
			// non-owned vehicle). The handler has already emitted the
			// typed error frame and closed the socket; just exit.
			return
		}
	}
}

// enqueue adds a message to the client's send buffer. If the buffer is
// full, it drops the oldest message to make room (drop-oldest policy).
// Returns true if a message was dropped.
func (c *Client) enqueue(msg []byte) bool {
	select {
	case c.send <- msg:
		return false
	default:
		// Buffer full — drop the oldest message.
		select {
		case <-c.send:
		default:
		}
		// Now try again. This should always succeed because we just
		// drained one slot (or the channel was consumed concurrently).
		select {
		case c.send <- msg:
		default:
			// Extremely unlikely race; just drop the new message.
		}
		return true
	}
}

// hasVehicle reports whether this client is authorized AND currently
// subscribed for the given vehicle ID. allVehicles=true (dev-mode
// NoopAuthenticator) short-circuits to true. Otherwise the vehicleID
// must be in the per-client subscription set, which is initialized
// from vehicleIDs at handshake and modified by subscribe/unsubscribe
// (DV-07 / MYR-46). An empty vehicleIDs slice with allVehicles=false
// means deny-all (NFR-3.21).
func (c *Client) hasVehicle(vehicleID string) bool {
	if c.allVehicles {
		return true
	}
	c.subMu.RLock()
	_, ok := c.subscribed[vehicleID]
	c.subMu.RUnlock()
	return ok
}

// owns reports whether the client was authorized for vehicleID at
// handshake time. Used by the subscribe handler to gate the
// permission_denied path before mutating the subscription set, so the
// ownership check is independent of the current subscription state.
func (c *Client) owns(vehicleID string) bool {
	if c.allVehicles {
		return true
	}
	return slices.Contains(c.vehicleIDs, vehicleID)
}

// subscribe adds vehicleID to the active subscription set. Caller MUST
// have verified ownership (Client.owns) first — the typed error frame
// for vehicle_not_owned is emitted by the readPump dispatcher, not
// here. Idempotent.
func (c *Client) subscribe(vehicleID string) {
	c.subMu.Lock()
	c.subscribed[vehicleID] = struct{}{}
	c.subMu.Unlock()
}

// unsubscribe removes vehicleID from the active subscription set.
// Idempotent: removing an already-absent ID is a no-op. Does NOT
// require ownership — a subscribed-but-since-revoked vehicle should
// still be removable so the client can drain the set on logout.
func (c *Client) unsubscribe(vehicleID string) {
	c.subMu.Lock()
	delete(c.subscribed, vehicleID)
	c.subMu.Unlock()
}

// writeMessage writes a single message to the WebSocket with a timeout.
func (c *Client) writeMessage(ctx context.Context, msg []byte, timeout time.Duration) error {
	writeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if err := c.conn.Write(writeCtx, websocket.MessageText, msg); err != nil {
		return fmt.Errorf("client.writeMessage(user=%s): %w", c.userID, err)
	}
	return nil
}

// isNormalClose reports whether the error represents a normal WebSocket
// closure (client disconnecting cleanly or context cancelled).
func isNormalClose(err error) bool {
	if err == context.Canceled { //nolint:errorlint // exact sentinel match intentional
		return true
	}
	status := websocket.CloseStatus(err)
	return status == websocket.StatusNormalClosure ||
		status == websocket.StatusGoingAway
}
