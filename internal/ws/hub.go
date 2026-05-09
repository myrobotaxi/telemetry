package ws

import (
	"log/slog"
	"sync"

	"github.com/tnando/my-robo-taxi-telemetry/internal/mask"
)

// Hub manages all connected WebSocket clients. It provides thread-safe
// registration, unregistration, and broadcast to authorized clients.
type Hub struct {
	clients map[*Client]struct{}
	mu      sync.RWMutex
	logger  *slog.Logger
	metrics HubMetrics
	stopped bool

	// Mask-audit fields (rest-api.md §5.3, MYR-71). All optional —
	// a nil auditEmitter disables emit and EmitAsync becomes a
	// no-op. Test wiring leaves them nil; production wiring fills
	// them via WithMaskAudit. The masked-broadcast surface (this
	// option, BroadcastMasked, buildRoleFrames, maybeEmitAuditWS,
	// nextFrameSeq, and frameCounters) lives in hub_masked.go.
	auditEmitter mask.AuditEmitter
	auditMetrics mask.AuditMetrics

	// Per-vehicle monotonic frame counter feeding ShouldAuditWS as
	// frameSeq input. sync.Map keyed by vehicleID -> *atomic.Uint64
	// so the per-vehicle increment is lock-free on the hot path.
	frameCounters sync.Map
}

// HubOption configures optional Hub behavior. Following the v1 SDK
// pattern (cmd/telemetry-server/main.go's WithMask), options are
// composable, idempotent, and default to a quiet no-op when omitted.
type HubOption func(*Hub)

// NewHub creates a Hub ready to accept client registrations.
func NewHub(logger *slog.Logger, metrics HubMetrics, opts ...HubOption) *Hub {
	h := &Hub{
		clients: make(map[*Client]struct{}),
		logger:  logger,
		metrics: metrics,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// Register adds an authenticated client to the hub.
func (h *Hub) Register(client *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if h.stopped {
		return
	}

	h.clients[client] = struct{}{}
	count := len(h.clients)
	h.metrics.SetConnectedClients(count)

	h.logger.Info("client registered",
		slog.String("user_id", client.userID),
		slog.Int("vehicle_count", len(client.vehicleIDs)),
		slog.Int("total_clients", count),
	)
}

// Unregister removes a client from the hub and closes its send channel.
func (h *Hub) Unregister(client *Client) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, ok := h.clients[client]; !ok {
		return
	}

	delete(h.clients, client)
	close(client.send)
	count := len(h.clients)
	h.metrics.SetConnectedClients(count)

	h.logger.Info("client unregistered",
		slog.String("user_id", client.userID),
		slog.Int("total_clients", count),
	)
}

// Broadcast sends a message to all clients authorized for the given
// vehicleID. Slow clients whose send buffers are full have their oldest
// message dropped.
//
// Note: this raw fan-out path is appropriate ONLY for messages whose
// payload contains no role-restricted fields (e.g., drive_started /
// drive_ended / connectivity in v1). For vehicle_update — where the
// per-resource mask matrix in rest-api.md §5.2 may strip fields per
// role — use BroadcastMasked. See websocket-protocol.md §4.6.
func (h *Hub) Broadcast(vehicleID string, msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for client := range h.clients {
		if !client.hasVehicle(vehicleID) {
			continue
		}
		if dropped := client.enqueue(msg); dropped {
			h.metrics.IncMessagesDropped()
			h.logger.Debug("dropped message for slow client",
				slog.String("user_id", client.userID),
				slog.String("vehicle_id", vehicleID),
			)
		}
	}
}

// BroadcastAll sends a message to all connected clients regardless of
// vehicle authorization. Used for heartbeats.
func (h *Hub) BroadcastAll(msg []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()

	for client := range h.clients {
		if dropped := client.enqueue(msg); dropped {
			h.metrics.IncMessagesDropped()
		}
	}
}

// Stop closes all client connections and prevents new registrations.
func (h *Hub) Stop() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.stopped = true
	for client := range h.clients {
		close(client.send)
		delete(h.clients, client)
	}
	h.metrics.SetConnectedClients(0)
	h.logger.Info("hub stopped")
}

// ClientCount returns the number of connected clients.
func (h *Hub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// ipConnectionCount returns the number of active connections from the given IP.
func (h *Hub) ipConnectionCount(ip string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	count := 0
	for client := range h.clients {
		if client.remoteAddr == ip {
			count++
		}
	}
	return count
}
