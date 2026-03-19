package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// HandlerConfig holds tuning parameters for the WebSocket handler.
type HandlerConfig struct {
	// AuthTimeout is how long the handler waits for the client to send
	// an auth message after the WebSocket upgrade. Default: 5s.
	AuthTimeout time.Duration

	// WriteTimeout is the per-message write deadline. Default: 10s.
	WriteTimeout time.Duration
}

// Handler returns an http.Handler that upgrades HTTP connections to
// WebSocket and manages the client lifecycle: auth handshake, read/write
// pumps, and cleanup on disconnect.
func (h *Hub) Handler(auth Authenticator, cfg HandlerConfig) http.Handler {
	cfg = applyHandlerDefaults(cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/ws", func(w http.ResponseWriter, r *http.Request) {
		h.handleUpgrade(w, r, auth, cfg)
	})
	return mux
}

// handleUpgrade performs the WebSocket upgrade, runs the auth handshake,
// and starts the read/write pumps.
func (h *Hub) handleUpgrade(w http.ResponseWriter, r *http.Request, auth Authenticator, cfg HandlerConfig) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
	if err != nil {
		h.logger.Error("websocket accept failed",
			slog.Any("error", err),
			slog.String("remote_addr", r.RemoteAddr),
		)
		return
	}

	client := newClient(conn, h, h.logger)

	// Authenticate: the client must send an auth message within the timeout.
	if err := h.authenticateClient(r.Context(), client, auth, cfg); err != nil {
		h.metrics.IncAuthFailures()
		h.logger.Warn("authentication failed",
			slog.Any("error", err),
			slog.String("remote_addr", r.RemoteAddr),
		)
		_ = conn.Close(websocket.StatusPolicyViolation, "authentication failed")
		return
	}

	// Client authenticated — register and start pumps.
	h.Register(client)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Write pump runs in a separate goroutine; read pump blocks this one.
	go client.writePump(ctx, cfg.WriteTimeout)

	// readPump blocks until the client disconnects.
	client.readPump(ctx)

	// Client disconnected — clean up.
	cancel()
	h.Unregister(client)
}

// authenticateClient waits for the auth message, validates the token,
// and populates the client's userID and vehicleIDs.
func (h *Hub) authenticateClient(ctx context.Context, client *Client, auth Authenticator, cfg HandlerConfig) error {
	authCtx, cancel := context.WithTimeout(ctx, cfg.AuthTimeout)
	defer cancel()

	_, data, err := client.conn.Read(authCtx)
	if err != nil {
		return fmt.Errorf("hub.authenticateClient: read auth message: %w", ErrAuthTimeout)
	}

	var msg wsMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return fmt.Errorf("hub.authenticateClient: unmarshal: %w", err)
	}

	if msg.Type != msgTypeAuth {
		return fmt.Errorf("hub.authenticateClient: expected %q, got %q", msgTypeAuth, msg.Type)
	}

	var payload authPayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return fmt.Errorf("hub.authenticateClient: unmarshal auth payload: %w", err)
	}

	userID, err := auth.ValidateToken(authCtx, payload.Token)
	if err != nil {
		_ = sendError(authCtx, client.conn, errCodeAuthFailed, "invalid token", cfg.WriteTimeout)
		return fmt.Errorf("hub.authenticateClient: validate token: %w", err)
	}

	vehicleIDs, err := auth.GetUserVehicles(authCtx, userID)
	if err != nil {
		_ = sendError(authCtx, client.conn, errCodeAuthFailed, "failed to load vehicles", cfg.WriteTimeout)
		return fmt.Errorf("hub.authenticateClient: get vehicles(user=%s): %w", userID, err)
	}

	client.userID = userID
	client.vehicleIDs = vehicleIDs
	return nil
}

// sendError writes an error message to the WebSocket connection.
func sendError(ctx context.Context, conn *websocket.Conn, code, message string, timeout time.Duration) error {
	writeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	payload, err := json.Marshal(errorPayload{Code: code, Message: message})
	if err != nil {
		return fmt.Errorf("sendError: marshal payload: %w", err)
	}

	msg, err := json.Marshal(wsMessage{Type: msgTypeError, Payload: payload})
	if err != nil {
		return fmt.Errorf("sendError: marshal message: %w", err)
	}

	return conn.Write(writeCtx, websocket.MessageText, msg)
}

// applyHandlerDefaults fills in zero-value fields with sensible defaults.
func applyHandlerDefaults(cfg HandlerConfig) HandlerConfig {
	if cfg.AuthTimeout == 0 {
		cfg.AuthTimeout = 5 * time.Second
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = 10 * time.Second
	}
	return cfg
}
