package telemetry

import (
	"context"
	"log/slog"
	"time"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/mask"
)

// The VIEWER MERGE for GET /api/vehicles (MYR-91, delivered by MYR-184).
//
// Before this, the list endpoint had a standing note saying viewer-merged
// enumeration was "PLANNED" and every caller got only vehicles they owned —
// which meant a rider who redeemed an invite saw an empty garage. This file is
// the other half: shared vehicles are appended as `role: "viewer"` rows
// carrying their `sharePermission`, projected through the VIEWER mask.
//
// Owner rows are untouched. They are built by the same newVehicleSummary as
// before, projected through the same owner mask, and emitted first — a caller
// with no shares sees a byte-identical response to the pre-MYR-184 one.

// SharedVehicleLister returns the caller's viewer-shaped catalog rows.
// Implementations apply the accepted-grant join themselves, so an id the
// caller has no grant on — or holds only a SUSPENDED grant on (MYR-369) —
// yields no row: the id list narrows an access-checked query and never
// substitutes for the check.
type SharedVehicleLister interface {
	// ListSharedByUser returns every vehicle shared with the caller.
	ListSharedByUser(ctx context.Context, userID string) ([]SharedVehicleRow, error)
	// ListSharedByIDs narrows the same query to a specific set — what one
	// redemption granted.
	ListSharedByIDs(ctx context.Context, userID string, vehicleIDs []string) ([]SharedVehicleRow, error)
}

// SharedVehicleRow is a catalog row plus the RIDE CAPABILITY the caller holds
// over it (MYR-369). The row only exists because a LIVE accepted grant produced
// it — the lister's join excludes suspended grants — so there is no default to
// invent and no suspension to re-check here.
type SharedVehicleRow struct {
	VehicleCatalogRow
	AllowRides bool
}

// WithSharedVehicles enables the viewer merge on the list handler. Omitting it
// leaves the endpoint owner-only — the pre-MYR-184 behaviour — which is the
// fail-closed direction: a deployment that forgot to wire sharing under-reports
// rather than over-shares.
func WithSharedVehicles(shared SharedVehicleLister) VehiclesListOption {
	return func(h *VehiclesListHandler) {
		h.shared = shared
	}
}

// VehiclesListOption configures optional dependencies on VehiclesListHandler.
type VehiclesListOption func(*VehiclesListHandler)

// appendSharedRows fetches the caller's shared vehicles and appends them to an
// owner-row response as viewer rows.
//
// A failure to read the shared set is LOGGED AND SWALLOWED, deliberately: the
// caller's own cars are already in hand, and returning 500 for the whole
// catalog because the sharing join hiccuped would take a rider's own garage
// down with it. The degraded response is a strictly smaller set — never a
// wider one — so the failure mode is "a shared car is missing for a moment",
// not "a car appears that should not".
func (h *VehiclesListHandler) appendSharedRows(ctx context.Context, userID string, resp vehiclesListResponse) vehiclesListResponse {
	if h.shared == nil {
		return resp
	}

	rows, err := h.shared.ListSharedByUser(ctx, userID)
	if err != nil {
		h.logger.Error("vehicles list: shared-vehicle merge failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		return resp
	}
	// One clock reading for the whole merge, matching the owner half: both
	// halves of a single catalog response must judge every row's MYR-491 setup
	// state against the same instant.
	now := time.Now()
	for i := range rows {
		resp.Items = append(resp.Items, viewerSummaryMap(
			rows[i].VehicleCatalogRow,
			auth.ShareGrant{AllowRides: rows[i].AllowRides},
			now,
		))
	}
	return resp
}

// viewerSummaryMap projects one shared vehicle through the VIEWER
// VehicleSummary mask.
//
// The mask is the real gate, not the struct: a field added to vehicleSummary
// without a matching allow-list entry is dropped from viewer rows rather than
// silently leaked. Used by both the list merge and the redeem response so a
// redeemer cannot see one field more on the join screen than in their catalog a
// second later.
//
// The v1 viewer allow-list subtracts NOTHING from the owner list and adds
// `sharePermission` — including `name`, the owner-curated nickname the rider UI
// renders as "{Owner}'s {Vehicle}". That is not incidental: every field
// vehicle-summary.schema.json marks `required` must survive this projection, or
// the rows this function emits are invalid against the shape their own consumer
// decodes. Asserted in vehicles_list_viewer_schema_test.go.
func viewerSummaryMap(row VehicleCatalogRow, grant auth.ShareGrant, now time.Time) map[string]any {
	summary := newVehicleSummary(&row, auth.RoleViewer, grant, now)
	projected, _ := mask.Apply(
		summary.toMaskMap(),
		mask.For(mask.ResourceVehicleSummary, auth.RoleViewer),
	)
	return projected
}
