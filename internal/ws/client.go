package ws

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/coder/websocket"
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
	remoteAddr string
	send       chan []byte
	hub        *Hub
	logger     *slog.Logger
}

// newClient creates a Client that is not yet authenticated. The userID and
// vehicleIDs are populated after the auth handshake completes.
func newClient(conn *websocket.Conn, hub *Hub, logger *slog.Logger) *Client {
	return &Client{
		conn:   conn,
		send:   make(chan []byte, sendBufSize),
		hub:    hub,
		logger: logger,
	}
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

// readPump reads messages from the WebSocket. After authentication, we
// only need to keep reading to detect client disconnect and respond to
// pings. All client-sent messages after auth are ignored.
func (c *Client) readPump(ctx context.Context) {
	c.conn.SetReadLimit(readLimit)
	for {
		_, _, err := c.conn.Read(ctx)
		if err != nil {
			if !isNormalClose(err) {
				c.logger.Debug("read error",
					slog.String("user_id", c.userID),
					slog.Any("error", err),
				)
			}
			return
		}
		// Post-auth messages are ignored; the read is only to detect
		// disconnects and keep the connection alive.
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

// hasVehicle reports whether this client is authorized to receive updates
// for the given vehicle ID. A nil/empty vehicleIDs slice grants access to
// all vehicles (used by NoopAuthenticator in dev mode).
func (c *Client) hasVehicle(vehicleID string) bool {
	if len(c.vehicleIDs) == 0 {
		return true
	}
	for _, id := range c.vehicleIDs {
		if id == vehicleID {
			return true
		}
	}
	return false
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
