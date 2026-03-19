package ws

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sync/errgroup"
)

// HandlerConfig holds tuning parameters for the WebSocket handler.
type HandlerConfig struct {
	// AuthTimeout is how long the handler waits for the client to send
	// an auth message after the WebSocket upgrade. Default: 5s.
	AuthTimeout time.Duration

	// WriteTimeout is the per-message write deadline. Default: 10s.
	WriteTimeout time.Duration

	// OriginPatterns restricts which origins may connect. Supports glob
	// patterns (e.g., "https://*.myrobotaxi.app"). Empty means reject
	// all cross-origin requests (browser default-same-origin only).
	OriginPatterns []string

	// MaxConnectionsPerIP limits concurrent WebSocket connections from
	// a single IP address. Zero means no limit.
	MaxConnectionsPerIP int
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
	clientIP := resolveClientIP(r)

	// Per-IP connection limit.
	if cfg.MaxConnectionsPerIP > 0 {
		if h.ipConnectionCount(clientIP) >= cfg.MaxConnectionsPerIP {
			h.logger.Warn("connection rate limited",
				slog.String("remote_addr", clientIP),
			)
			http.Error(w, "too many connections", http.StatusTooManyRequests)
			return
		}
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: cfg.OriginPatterns,
	})
	if err != nil {
		h.logger.Error("websocket accept failed",
			slog.Any("error", err),
			slog.String("remote_addr", clientIP),
		)
		return
	}

	client := newClient(conn, h, h.logger)
	client.remoteAddr = clientIP

	// Authenticate: the client must send an auth message within the timeout.
	if err := h.authenticateClient(r.Context(), client, auth, cfg); err != nil {
		h.metrics.IncAuthFailures()
		h.logger.Warn("authentication failed",
			slog.Any("error", err),
			slog.String("remote_addr", clientIP),
		)
		errCode := errCodeAuthFailed
		if errors.Is(err, ErrAuthTimeout) {
			errCode = errCodeAuthTimeout
		}
		errCtx, errCancel := context.WithTimeout(context.Background(), cfg.WriteTimeout)
		_ = sendError(errCtx, conn, errCode, err.Error(), cfg.WriteTimeout)
		errCancel()
		_ = conn.Close(websocket.StatusPolicyViolation, "authentication failed")
		return
	}

	// Client authenticated — register and start pumps.
	h.Register(client)

	ctx, cancel := context.WithCancel(r.Context())

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		client.writePump(gctx, cfg.WriteTimeout)
		return nil
	})
	g.Go(func() error {
		client.readPump(gctx)
		return nil
	})

	_ = g.Wait()
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
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("hub.authenticateClient: %w", ErrAuthTimeout)
		}
		return fmt.Errorf("hub.authenticateClient: read auth message: %w", err)
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

	if err = conn.Write(writeCtx, websocket.MessageText, msg); err != nil {
		return fmt.Errorf("sendError: write: %w", err)
	}
	return nil
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

// resolveClientIP returns the client's IP address, preferring the
// X-Forwarded-For header (leftmost entry) when behind a reverse proxy.
func resolveClientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		// X-Forwarded-For: client, proxy1, proxy2 — take the leftmost.
		if ip, _, ok := strings.Cut(fwd, ","); ok {
			return strings.TrimSpace(ip)
		}
		return strings.TrimSpace(fwd)
	}
	return r.RemoteAddr
}
