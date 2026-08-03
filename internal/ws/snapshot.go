package ws

import (
	"context"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/mask"
)

// VehicleSnapshot is the full set of persisted vehicle fields for one
// vehicle, keyed by WIRE field name (docs/contracts/vehicle-state-schema.md
// §1.1) rather than internal telemetry name. It mirrors the shape the REST
// GET /api/vehicles/{vehicleId}/snapshot endpoint returns
// (internal/telemetry/vehicle_snapshot_types.go) so a WS client that never
// calls REST still sees the DB-only catalog fields (model, year, color)
// and the last-known values of fields Tesla only re-emits on change
// (estimatedRange, fsdMilesSinceReset). Timestamp is the DB row's
// lastUpdated column, RFC 3339 UTC.
type VehicleSnapshot struct {
	Fields    map[string]any
	Timestamp string
}

// VehicleSnapshotReader loads the persisted snapshot row for a vehicle ID.
// Defined at the consumer site (internal/ws), per this repo's "interfaces
// at consumer site" convention — implemented by an adapter over
// store.VehicleRepo.GetByID in cmd/telemetry-server, kept separate from
// the near-identical telemetry.VehicleSnapshotReader (which backs the REST
// endpoint) so internal/ws does not import internal/telemetry.
type VehicleSnapshotReader interface {
	GetVehicleSnapshot(ctx context.Context, vehicleID string) (VehicleSnapshot, error)
}

// wireGroupMembers maps each atomic group to its WIRE field names, as
// opposed to atomicGroupMembers (atomic_groups.go), which is keyed by
// internal telemetry field names for the live-broadcast path. Snapshot
// fields are already wire-shaped (sourced directly from the DB row via
// VehicleSnapshotReader), so partitioning happens against this table
// instead. Source of truth: vehicle-state-schema.md §2, websocket-protocol.md
// §3.2.
var wireGroupMembers = map[atomicGroupID][]string{
	groupNavigation: {
		"destinationName", "destinationAddress",
		"destinationLatitude", "destinationLongitude",
		"originLatitude", "originLongitude",
		"etaMinutes", "tripDistanceRemaining", "navRouteCoordinates",
	},
	groupCharge: {"chargeLevel", "chargeState", "estimatedRange", "timeToFull"},
	groupGPS:    {"latitude", "longitude", "heading"},
	groupGear:   {"gearPosition", "status"},
}

// snapshotGroupOrder fixes the emission order of the per-group snapshot
// frames so tests and packet captures are deterministic. The order has no
// wire-contract meaning beyond that — each frame is independently valid.
var snapshotGroupOrder = []atomicGroupID{groupNavigation, groupCharge, groupGPS, groupGear}

// partitionSnapshotFields splits a flat wire-field map into one map per
// atomic group plus a remainder of ungrouped ("individual", per
// vehicle-state-schema.md §1.1) fields such as model/year/color and
// fsdMilesSinceReset. This is required so the snapshot obeys the same "at
// most one atomic group per frame" rule (websocket-protocol.md §3.2) that
// the live broadcast path enforces — dumping the full snapshot into a
// single frame would mix navigation/charge/gps/gear members together and
// be a contract violation that conformant SDKs are required to discard.
func partitionSnapshotFields(fields map[string]any) (groups map[atomicGroupID]map[string]any, ungrouped map[string]any) {
	memberOf := make(map[string]atomicGroupID, len(wireGroupMembers)*4)
	for group, names := range wireGroupMembers {
		for _, name := range names {
			memberOf[name] = group
		}
	}

	groups = make(map[atomicGroupID]map[string]any, len(wireGroupMembers))
	ungrouped = make(map[string]any, len(fields))
	for name, val := range fields {
		group, ok := memberOf[name]
		if !ok {
			ungrouped[name] = val
			continue
		}
		if groups[group] == nil {
			groups[group] = make(map[string]any, len(wireGroupMembers[group]))
		}
		groups[group][name] = val
	}
	return groups, ungrouped
}

// sendSnapshot fetches the persisted snapshot for vehicleID and unicasts
// it to client as one vehicle_update frame per atomic group present (plus
// one for the ungrouped individual fields), so the client sees the full
// known state -- including model/year/color and the current
// chargeLevel/estimatedRange pair and fsdMilesSinceReset -- the moment it
// subscribes, without waiting for live Tesla telemetry to arrive
// (MYR-137). Because estimatedRange and chargeLevel are both members of
// groupCharge and are read from the same DB row, they always land in the
// same frame here, satisfying the "range accompanies chargeLevel" rule by
// construction.
//
// A nil reader (test wiring that doesn't need this, or a deploy where the
// adapter failed to wire) or a fetch error is non-fatal: it is logged and
// the connection proceeds on live telemetry only, matching the resilience
// posture of the rest of the broadcast path (e.g. VIN-resolution failures
// are logged-and-skipped rather than closing the connection).
func (h *Hub) sendSnapshot(ctx context.Context, client *Client, vehicleID string, writeTimeout time.Duration) {
	if h.snapshots == nil {
		return
	}

	fetchCtx, cancel := context.WithTimeout(ctx, writeTimeout)
	snap, err := h.snapshots.GetVehicleSnapshot(fetchCtx, vehicleID)
	cancel()
	if err != nil {
		h.logger.Warn("hub.sendSnapshot: fetch failed, client will only see live telemetry",
			slog.String("user_id", client.userID),
			slog.String("vehicle_id", vehicleID),
			slog.Any("error", err),
		)
		return
	}

	role := client.roleFor(vehicleID)
	groups, ungrouped := partitionSnapshotFields(snap.Fields)

	// lastUpdated is itself an ungrouped wire field (vehicle-state-schema.md
	// §4) and the live broadcast path always adds it to `fields` alongside
	// the envelope-level `timestamp` (see handleTelemetry / flushGroup) —
	// match that convention here rather than only setting the envelope.
	ungrouped["lastUpdated"] = snap.Timestamp

	h.enqueueSnapshotFrame(client, vehicleID, role, ungrouped, snap.Timestamp)
	for _, group := range snapshotGroupOrder {
		if fields := groups[group]; len(fields) > 0 {
			h.enqueueSnapshotFrame(client, vehicleID, role, fields, snap.Timestamp)
		}
	}
}

// enqueueSnapshotFrame projects one group's fields through the client's
// role mask and, if anything survives projection, marshals and enqueues a
// vehicle_update frame directly onto the client's own send channel.
// Unlike BroadcastMasked this is a unicast: it must not fan out to every
// other client subscribed to vehicleID, or every new subscriber would
// re-deliver a stale snapshot to clients who are already caught up.
func (h *Hub) enqueueSnapshotFrame(client *Client, vehicleID string, role auth.Role, fields map[string]any, timestamp string) {
	projected, _ := mask.Apply(fields, mask.For(mask.ResourceVehicleState, role))
	if !mask.IsSubstantive(projected) {
		// Empty-payload suppression, mirroring BroadcastMasked: a role
		// whose mask strips every field in this group must not see an
		// empty vehicle_update.
		//
		// MYR-435 widened this alongside BroadcastMasked, and deliberately
		// through the SAME predicate. sendSnapshot sets
		// ungrouped["lastUpdated"] before projecting, exactly as the live
		// path does, so this call site had the identical latent hole: a
		// replayed snapshot whose ungrouped fields are all owner-only would
		// have delivered a bare freshness frame to a viewer. Two copies of
		// this rule is how one of them would rot.
		return
	}
	frame, err := marshalVehicleUpdate(vehicleID, projected, timestamp)
	if err != nil {
		h.logger.Warn("hub.enqueueSnapshotFrame: marshal failed",
			slog.String("user_id", client.userID),
			slog.String("vehicle_id", vehicleID),
			slog.Any("error", err),
		)
		return
	}
	if dropped := client.enqueue(frame); dropped {
		h.metrics.IncMessagesDropped()
	}
}
