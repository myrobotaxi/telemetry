package ws

import (
	"log/slog"
	"sync"

	"github.com/myrobotaxi/telemetry/internal/mask"
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

// closeCodeVehicleAccessRevoked is the WebSocket close code emitted when
// a client's vehicle subscription is forcibly closed because the
// underlying Vehicle row was deleted (FR-10.1 / data-lifecycle.md §3.5,
// MYR-73). Reuses code 4002 from the typed-error catalog (see
// websocket-protocol.md §6.2 / DV-09 target). Identical wire code as
// the §6.1.1 vehicle_not_owned path because the SDK's reaction is the
// same: surface to UI, do not auto-retry the same vehicleId.
const closeCodeVehicleAccessRevoked = 4002

// vehicleAccessRevokedReason is the close-frame reason string emitted
// alongside closeCodeVehicleAccessRevoked. Per the issue spec, the
// reason is "vehicle access revoked" (lowercase, no period).
const vehicleAccessRevokedReason = "vehicle access revoked"

// RemoveVehicle closes every connected client whose authorized
// vehicle set or active subscription set contains vehicleID. Each
// affected client receives a WebSocket close with code 4002 and reason
// "vehicle access revoked", and the per-deletion counter
// `ws_close_user_deletion_total` is incremented once per closed
// session.
//
// Used by the data-lifecycle.md §3.5 cleanup path: the Postgres
// `vehicle_deleted` LISTEN goroutine (internal/store/notify_listener.go)
// publishes a VehicleDeletedEvent which the hub's subscription invokes
// here. RemoveVehicle is also safe to call directly from tests.
//
// Implementation notes:
//   - We close the *connection* (not just the send channel) so the
//     SDK observes the close code, not just an EOF. The Unregister
//     path runs naturally when the readPump exits on the closed conn.
//   - allVehicles=true clients (dev-mode wildcard) are closed too —
//     they were authorized for every vehicle, including this one.
//   - We snapshot the affected client set under the read lock, then
//     close outside the lock so we do not hold h.mu while a slow
//     close handshake is in flight.
func (h *Hub) RemoveVehicle(vehicleID string) {
	if vehicleID == "" {
		return
	}

	affected := h.collectAffectedClients(vehicleID)
	if len(affected) == 0 {
		return
	}

	for _, client := range affected {
		if client.conn != nil {
			_ = client.conn.Close(closeCodeVehicleAccessRevoked, vehicleAccessRevokedReason)
		}
		h.metrics.IncCloseUserDeletion()
		h.logger.Info("closing client: vehicle access revoked",
			slog.String("user_id", client.userID),
			slog.String("vehicle_id", vehicleID),
			slog.Int("close_code", closeCodeVehicleAccessRevoked),
		)
	}
}

// collectAffectedClients walks the hub's client set under RLock and
// returns every client that owns vehicleID, has it in its active
// subscription set, OR has the dev-mode wildcard flag set. The
// returned slice is safe to mutate outside the lock.
func (h *Hub) collectAffectedClients(vehicleID string) []*Client {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var affected []*Client
	for client := range h.clients {
		if clientAuthorizedForVehicle(client, vehicleID) {
			affected = append(affected, client)
		}
	}
	return affected
}

// clientAuthorizedForVehicle reports whether the client's handshake-time
// authorization or current subscription set covers vehicleID. We close
// in either case: an authorized-but-not-currently-subscribed client
// will start receiving frames the moment they re-subscribe, so the
// revocation signal must reach them now.
func clientAuthorizedForVehicle(c *Client, vehicleID string) bool {
	if c == nil {
		return false
	}
	if c.allVehicles {
		return true
	}
	for _, vid := range c.vehicleIDs {
		if vid == vehicleID {
			return true
		}
	}
	c.subMu.RLock()
	_, ok := c.subscribed[vehicleID]
	c.subMu.RUnlock()
	return ok
}
