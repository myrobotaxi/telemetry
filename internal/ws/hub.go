package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

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

	// Mask-audit fields (rest-api.md §5.3). All optional — a nil
	// auditEmitter disables emit and EmitAsync becomes a no-op. Test
	// wiring leaves them nil; production wiring fills them via
	// HubOption.
	auditEmitter mask.AuditEmitter
	auditMetrics mask.AuditMetrics

	// Per-vehicle monotonic frame counter feeding ShouldAuditWS as
	// frameSeq input. Per rest-api.md §5.3, until DV-02 lands the hub
	// uses an in-process counter — distinct vehicles get distinct
	// streams, distinct frames within a stream get distinct counter
	// values, and the deterministic hash distributes the 1% sample
	// across them. sync.Map keyed by vehicleID -> *atomic.Uint64 so
	// the per-vehicle increment is lock-free on the hot path.
	frameCounters sync.Map
}

// HubOption configures optional Hub behavior. Following the v1 SDK
// pattern (cmd/telemetry-server/main.go's WithMask), options are
// composable, idempotent, and default to a quiet no-op when omitted.
type HubOption func(*Hub)

// WithMaskAudit attaches a mask-audit emitter and metrics to the hub.
// When configured, the hub emits an AuditEntry per (vehicleID, role,
// frame) where the role's mask removed at least one field, sampled at
// the 1% rate computed by mask.ShouldAuditWS. emitter MAY be nil — in
// which case this option is a no-op (defensive: a misconfigured wiring
// path should not crash the hot path).
func WithMaskAudit(emitter mask.AuditEmitter, metrics mask.AuditMetrics) HubOption {
	return func(h *Hub) {
		if emitter == nil {
			return
		}
		h.auditEmitter = emitter
		if metrics == nil {
			metrics = mask.NoopAuditMetrics{}
		}
		h.auditMetrics = metrics
	}
}

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

// nextFrameSeq increments and returns the per-vehicle monotonic frame
// counter used as ShouldAuditWS input. The first call for a given
// vehicleID returns 1; subsequent calls return 2, 3, ... A sync.Map
// LoadOrStore on the first call avoids the race between two goroutines
// observing zero and both creating a counter — the second goroutine
// loses the LoadOrStore race, throws away its counter, and uses the
// winner's.
func (h *Hub) nextFrameSeq(vehicleID string) uint64 {
	v, ok := h.frameCounters.Load(vehicleID)
	if !ok {
		v, _ = h.frameCounters.LoadOrStore(vehicleID, new(atomic.Uint64))
	}
	return v.(*atomic.Uint64).Add(1)
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
// by websocket-protocol.md §4.6. It pre-marshals one frame per
// active role for this vehicle (the set of distinct roles held by
// currently-subscribed clients), then fans out the role-appropriate
// bytes to each authorized client based on that client's per-vehicle
// role (populated at handshake time).
//
// Marshal cost is O(|active roles for this vehicle|), bounded above by
// O(|v1 roles|). Fan-out is O(|clients|). Per §4.6 "empty-payload
// suppression": if a role's mask projects the payload down to zero
// fields, no frame is emitted for clients holding that role — a viewer
// who shouldn't even know an event happened on the vehicle MUST NOT
// see an empty vehicle_update.
//
// Clients whose roleFor(vehicleID) returns the empty Role("") sentinel
// (e.g., handshake-time ResolveRole failed) receive nothing — the
// fail-closed behavior described in rest-api.md §5.
//
// Active-role pre-pass: we walk h.clients ONCE under RLock to collect
// the distinct roles actually subscribed for this vehicleID, then only
// marshal frames for those roles. Skipping unused roles matters in
// practice today (every connection in v1 is owner — viewers via the
// Invite flow haven't shipped yet) and the savings grow with FR-5.5's
// third role. PR #195 review suggestion #1.
func (h *Hub) BroadcastMasked(
	vehicleID string,
	resource mask.ResourceType,
	timestamp string,
	payload map[string]any,
) {
	if len(payload) == 0 {
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	// Pass 1: collect the distinct set of roles held by clients
	// subscribed to this vehicle. O(|clients|).
	activeRoles := make(map[auth.Role]struct{}, len(v1Roles))
	for client := range h.clients {
		if !client.hasVehicle(vehicleID) {
			continue
		}
		activeRoles[client.roleFor(vehicleID)] = struct{}{}
	}
	if len(activeRoles) == 0 {
		return
	}

	// Per rest-api.md §5.3: the audit emit's frameSeq is per-vehicle.
	// Bump once per BroadcastMasked call so every role that processes
	// this frame sees the same seq value — the role itself goes into
	// the hash too, so distinct roles still get distinct sampling
	// decisions for the same frame.
	frameSeq := h.nextFrameSeq(vehicleID)

	// Pass 2: marshal only the frames we will actually use.
	framesByRole := h.buildRoleFrames(vehicleID, resource, timestamp, payload, activeRoles, frameSeq)
	if len(framesByRole) == 0 {
		return
	}

	// Pass 3: fan out under the same RLock so the client set we marshaled
	// for is the same client set we deliver to (no torn snapshot).
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
// frames for a single broadcast. Iterates ONLY the activeRoles set
// (distinct roles held by clients subscribed to this vehicle) to avoid
// wasting marshal cost on roles no one is listening with. Returns a
// map[role]frame; an entry with a nil value indicates empty-payload
// suppression for that role. Marshal failures fall back to "no frame
// for that role" rather than an error return — a single role's marshal
// failure must not poison the broadcast for other roles.
//
// Audit emit (MYR-71, rest-api.md §5.3): for each role where the
// mask removed at least one field AND mask.ShouldAuditWS samples in,
// the hub fires a non-blocking AuditEntry insert via mask.EmitAsync.
// The emit is per (vehicleID, role, frame) at the hub layer rather
// than per client — this keeps audit volume proportional to vehicle
// activity, not viewer count. Failures log slog.Warn and increment
// audit_log_write_failures_total; they MUST NOT drop the frame.
func (h *Hub) buildRoleFrames(
	vehicleID string,
	resource mask.ResourceType,
	timestamp string,
	payload map[string]any,
	activeRoles map[auth.Role]struct{},
	frameSeq uint64,
) map[auth.Role][]byte {
	frames := make(map[auth.Role][]byte, len(activeRoles))
	for role := range activeRoles {
		m := mask.For(resource, role)
		projected, fieldsMasked := mask.Apply(payload, m)

		// Audit-log emit per rest-api.md §5.3. Sampled at 1% by
		// deterministic hash. Skipped if the role didn't actually
		// strip any fields (the contract gates emit on
		// "removed at least one field").
		h.maybeEmitAuditWS(vehicleID, role, frameSeq, fieldsMasked)

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

// maybeEmitAuditWS evaluates the audit-emit gate for one (vehicleID,
// role, frame) and fires a non-blocking insert if both the
// "fieldsMasked is non-empty" precondition and the 1% sampler agree.
// Pulled out of buildRoleFrames so the hot path stays linear and the
// gate logic is independently testable.
func (h *Hub) maybeEmitAuditWS(
	vehicleID string,
	role auth.Role,
	frameSeq uint64,
	fieldsMasked []string,
) {
	if h.auditEmitter == nil {
		return
	}
	if len(fieldsMasked) == 0 {
		return
	}
	if !mask.ShouldAuditWS(vehicleID, role, frameSeq) {
		return
	}
	// userID is empty for WS — the audit emit is per (vehicleID, role,
	// frame) at the hub, not per client. The targetID column carries
	// the vehicleID; the role is in metadata; that's the canonical
	// shape from rest-api.md §5.3 ("The audit emit happens once per
	// (vehicleId, role, frame) at the hub layer, not per client").
	entry, err := mask.BuildEntry(
		"",
		mask.TargetWSBroadcast,
		vehicleID,
		role,
		mask.AuditChannelWS,
		fieldsMasked,
		"",
	)
	if err != nil {
		// BuildEntry only fails on programmer errors (empty
		// fieldsMasked, marshal panic). Log and skip — the frame
		// itself still flies.
		h.logger.Warn("hub.maybeEmitAuditWS: BuildEntry failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("role", role.String()),
			slog.String("error", err.Error()),
		)
		return
	}
	mask.EmitAsync(context.Background(), h.auditEmitter, h.auditMetrics, h.logger, entry)
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
