package ws

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	"github.com/tnando/my-robo-taxi-telemetry/internal/auth"
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
}

// NewHub creates a Hub ready to accept client registrations.
func NewHub(logger *slog.Logger, metrics HubMetrics) *Hub {
	return &Hub{
		clients: make(map[*Client]struct{}),
		logger:  logger,
		metrics: metrics,
	}
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

// BroadcastMasked is the per-role projection broadcast path required
// by websocket-protocol.md §4.6. It pre-marshals one frame per v1 role
// using the field mask from internal/mask, then fans out the
// role-appropriate bytes to each authorized client based on that
// client's per-vehicle role (populated at handshake time).
//
// Marshal cost is O(|roles|) per call; fan-out is O(|clients|). Per
// §4.6 "empty-payload suppression": if a role's mask projects the
// payload down to zero fields, no frame is emitted for clients holding
// that role — a viewer who shouldn't even know an event happened on
// the vehicle MUST NOT see an empty vehicle_update.
//
// Clients whose roleFor(vehicleID) returns the empty Role("") sentinel
// (e.g., handshake-time ResolveRole failed) receive nothing — the
// fail-closed behavior described in rest-api.md §5.
func (h *Hub) BroadcastMasked(
	vehicleID string,
	resource mask.ResourceType,
	timestamp string,
	payload map[string]any,
) {
	if len(payload) == 0 {
		return
	}

	framesByRole := buildRoleFrames(vehicleID, resource, timestamp, payload)
	if len(framesByRole) == 0 {
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	for client := range h.clients {
		if !client.hasVehicle(vehicleID) {
			continue
		}
		role := client.roleFor(vehicleID)
		frame, ok := framesByRole[role]
		if !ok || frame == nil {
			// Empty-payload suppression OR unknown role -> deny-all.
			continue
		}
		if dropped := client.enqueue(frame); dropped {
			h.metrics.IncMessagesDropped()
			h.logger.Debug("dropped masked message for slow client",
				slog.String("user_id", client.userID),
				slog.String("vehicle_id", vehicleID),
				slog.String("role", role.String()),
			)
		}
	}
}

// v1Roles enumerates the roles for which the hub pre-marshals a frame
// per call to BroadcastMasked. Two roles in v1 (FR-5.4); FR-5.5 adds
// limited_viewer in a later release.
var v1Roles = []auth.Role{auth.RoleOwner, auth.RoleViewer}

// buildRoleFrames produces the per-role pre-marshaled vehicle_update
// frames for a single broadcast. Returns a map[role]frame; an entry
// with a nil value indicates empty-payload suppression for that role.
// Marshal failures fall back to "no frame for that role" rather than
// an error return — a single role's marshal failure must not poison
// the broadcast for other roles.
//
// TODO(MYR-XX audit-log): when AuditLog table exists, for each role
// where len(fieldsMasked) > 0 AND mask.ShouldAuditWS(vehicleID, role,
// frameSeq) == true, emit an audit entry per rest-api.md §5.3. The
// AuditLog migration is deferred (data-lifecycle.md §4 schema doesn't
// exist in Prisma yet — same cross-repo pattern as MYR-41's
// chargeState/timeToFull migration). fieldsMasked carries the list of
// removed field names (P0 — names only, never values).
func buildRoleFrames(
	vehicleID string,
	resource mask.ResourceType,
	timestamp string,
	payload map[string]any,
) map[auth.Role][]byte {
	frames := make(map[auth.Role][]byte, len(v1Roles))
	for _, role := range v1Roles {
		m := mask.For(resource, role)
		projected, fieldsMasked := mask.Apply(payload, m)
		_ = fieldsMasked // see TODO above

		if len(projected) == 0 {
			// Empty-payload suppression per websocket-protocol.md §4.6.
			frames[role] = nil
			continue
		}
		frame, err := marshalVehicleUpdate(vehicleID, projected, timestamp)
		if err != nil {
			// Drop this role only; do not poison the broadcast.
			continue
		}
		frames[role] = frame
	}
	return frames
}

// marshalVehicleUpdate wraps a projected payload in the wsMessage
// envelope and returns the JSON bytes ready to enqueue.
func marshalVehicleUpdate(vehicleID string, fields map[string]any, timestamp string) ([]byte, error) {
	payloadBytes, err := json.Marshal(vehicleUpdatePayload{
		VehicleID: vehicleID,
		Fields:    fields,
		Timestamp: timestamp,
	})
	if err != nil {
		return nil, fmt.Errorf("marshalVehicleUpdate(vehicle=%s): payload: %w", vehicleID, err)
	}
	msg, err := json.Marshal(wsMessage{Type: msgTypeVehicleUpdate, Payload: payloadBytes})
	if err != nil {
		return nil, fmt.Errorf("marshalVehicleUpdate(vehicle=%s): envelope: %w", vehicleID, err)
	}
	return msg, nil
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
