package telemetry

import (
	"context"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/auth"
)

// The MEMBER MERGE for GET /api/vehicles (MYR-540).
//
// A group-ride joiner holds live access to the ride's vehicle — the WS
// handshake admits them through internal/auth's membership leg — but the
// catalog is what the app's watched-vehicle ladder resolves against, and a car
// the catalog omits is a car the app treats as gone. This file appends the
// vehicles of LIVE rides the caller has joined as `role: "viewer"` rows, after
// the owner and shared halves, so the three surfaces (catalog, access set,
// role resolution) name the same cars for the same caller.
//
// Membership rows carry the ZERO grant — viewer tier, no ride capability —
// matching resolveRidingMember on the WS side: riding in a car buys watching
// it, never booking it. And they are DEDUPLICATED against the rows already in
// hand: a caller who owns the car keeps their owner row, a caller with a share
// keeps the share's richer capability row, and the member row appears only
// when membership is the ONLY relationship — which is exactly the joiner this
// merge exists for.

// RideMemberVehicleLister returns viewer-shaped catalog rows for the vehicles
// of live group rides the caller has joined. Implementations apply the
// live-membership join themselves (the join is the access check), so a ride
// that has ended yields no row — access expires with the ride, with nothing to
// revoke.
type RideMemberVehicleLister interface {
	ListMemberVehiclesByUser(ctx context.Context, userID string) ([]VehicleCatalogRow, error)
}

// WithMemberVehicles enables the member merge on the list handler. Omitting it
// leaves the endpoint owner+shared — the pre-MYR-540 behaviour — which is the
// fail-closed direction: a deployment that forgot to wire it under-reports
// rather than over-shares.
func WithMemberVehicles(members RideMemberVehicleLister) VehiclesListOption {
	return func(h *VehiclesListHandler) {
		h.members = members
	}
}

// appendMemberRows fetches the caller's member vehicles and appends the ones
// no earlier row already covers.
//
// Failures are LOGGED AND SWALLOWED for the same reason appendSharedRows
// swallows them: the degraded response is a strictly smaller set, and a
// joiner's catalog hiccup must not take an owner's own garage down with it.
func (h *VehiclesListHandler) appendMemberRows(ctx context.Context, userID string, resp vehiclesListResponse) vehiclesListResponse {
	if h.members == nil {
		return resp
	}

	rows, err := h.members.ListMemberVehiclesByUser(ctx, userID)
	if err != nil {
		h.logger.Error("vehicles list: member-vehicle merge failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		return resp
	}
	if len(rows) == 0 {
		return resp
	}

	// The rows already in hand win: an owner row carries the owner mask and a
	// share row carries a real capability, and both say strictly more than a
	// membership does about the same car.
	seen := make(map[string]bool, len(resp.Items))
	for _, item := range resp.Items {
		if id, ok := item["vehicleId"].(string); ok {
			seen[id] = true
		}
	}

	// One clock reading for the appended rows, matching the other two halves:
	// every row of one catalog response judges its MYR-491 setup state against
	// the same instant.
	now := time.Now()
	for i := range rows {
		if seen[rows[i].ID] {
			continue
		}
		seen[rows[i].ID] = true
		resp.Items = append(resp.Items, viewerSummaryMap(rows[i], auth.ShareGrant{}, now))
	}
	return resp
}
