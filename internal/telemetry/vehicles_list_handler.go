package telemetry

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/mask"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// VehicleCatalogRow is the slim per-vehicle shape the list handler
// consumes from its VehicleLister dependency. Mirrors the subset of
// `store.Vehicle` fields the catalog response surfaces (no GPS, no
// nav, no climate). Defined here so the handler can stay decoupled
// from `internal/store` and avoid the import cycle that arises when
// `internal/telemetry` depends on `internal/store` (the telemetry
// package is already imported by store-adjacent code through
// `cmd/ops`).
type VehicleCatalogRow struct {
	ID    string
	VIN   string
	Name  string
	Model string
	Year  int
	Color string
	// LicensePlate is the owner-entered plate (MYR-286), an identity-row
	// field off the Prisma "Vehicle" column like Color/Name — not
	// telemetry. Empty string == not set.
	LicensePlate   string
	Status         string
	ChargeLevel    int
	EstimatedRange int
	LastUpdated    time.Time
	// HasActiveRide is derived read-time by the store's list query
	// (MYR-233), not persisted on the vehicle: true iff the car holds
	// an open INSTANT ride request (`accepted` / `enroute` / `arrived`,
	// `scheduled_for IS NULL`) — the same predicate the per-vehicle
	// accept guard races on.
	HasActiveRide bool

	// MYR-316 service window: the two RAW sources behind the single wire
	// field serviceEstimatedEndAt, LEFT JOINed from the Go-owned control-state
	// side table. ServiceETC (Tesla) takes precedence over
	// ServiceExpectedEndAt (owner-entered); the wire value is resolved from
	// these plus Status and is never emitted raw.
	ServiceETC           *time.Time
	ServiceExpectedEndAt *time.Time
}

// VehicleLister returns the catalog rows for vehicles owned by a
// user. The adapter in `cmd/telemetry-server` wires a real
// `store.VehicleRepo.ListByUser` into this interface and converts
// `[]store.Vehicle` → `[]VehicleCatalogRow` at the boundary.
type VehicleLister interface {
	ListByUser(ctx context.Context, userID string) ([]VehicleCatalogRow, error)
}

// VehiclesListHandler handles GET /api/vehicles. It validates the
// caller's JWT, enumerates the caller's owned vehicles via
// VehicleLister, projects each row through the per-role
// VehicleSummary mask, and returns the catalog.
//
// MYR-184 DELIVERED THE VIEWER MERGE this comment used to describe as
// PLANNED. Owned vehicles are emitted first under the owner mask; vehicles
// somebody has SHARED with the caller are then appended as `role: "viewer"`
// rows carrying their `sharePermission`, projected through the viewer mask
// (see vehicles_list_viewer.go). The old note said viewer-tier callers "receive
// an empty list in v1" and that the merge needed a Prisma-owned Invite table —
// neither is true: sharing is Go-owned end to end (go_vehicle_shares), and a
// rider who owns nothing now gets exactly the cars they were granted.
type VehiclesListHandler struct {
	auth     tokenValidator
	vehicles VehicleLister
	// shared supplies the MYR-184 viewer merge. Nil leaves the endpoint
	// owner-only (see vehicles_list_viewer.go).
	shared SharedVehicleLister
	logger *slog.Logger
}

// NewVehiclesListHandler creates a handler that serves the
// GET /api/vehicles list endpoint. Pass WithSharedVehicles to enable the
// MYR-184 viewer merge.
func NewVehiclesListHandler(
	tokens tokenValidator,
	vehicles VehicleLister,
	logger *slog.Logger,
	opts ...VehiclesListOption,
) *VehiclesListHandler {
	h := &VehiclesListHandler{
		auth:     tokens,
		vehicles: vehicles,
		logger:   logger,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// vehiclesListResponse is the envelope shape returned by the
// endpoint. Matches `VehiclesListResponse` in specs/rest.openapi.yaml.
// v1 emits only `items` — pagination fields are reserved per §7.0.
type vehiclesListResponse struct {
	Items []map[string]any `json:"items"`
}

// ServeHTTP handles GET /api/vehicles.
func (h *VehiclesListHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	token := extractBearerToken(r)
	if token == "" {
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "missing Authorization header")
		return
	}

	ctx := r.Context()

	userID, err := h.auth.ValidateToken(ctx, token)
	if err != nil {
		h.logger.Warn("vehicles list: invalid token",
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return
	}

	rows, err := h.vehicles.ListByUser(ctx, userID)
	if err != nil {
		h.logger.Error("vehicles list: ListByUser failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	// ListByUser returns vehicles where Vehicle.userId == userID, so every
	// row here is owned by the caller and gets the owner mask. Vehicles
	// somebody SHARED with the caller are appended separately as viewer rows
	// (MYR-184 / MYR-91 viewer merge) — owner rows first, then shared.
	resp := h.buildResponse(rows, auth.RoleOwner)
	resp = h.appendSharedRows(ctx, userID, resp)
	h.writeJSON(w, http.StatusOK, resp)
}

// buildResponse projects each Vehicle row through the per-role
// VehicleSummary mask and assembles the response envelope.
//
// Every row this function sees is OWNED by the caller, so they all get the
// RoleOwner mask (the identity for the owner allow-list). The viewer rows the
// merge appends are projected separately, in viewerSummaryMap — the per-row
// mask application here is what made that separation cheap.
func (h *VehiclesListHandler) buildResponse(rows []VehicleCatalogRow, role auth.Role) vehiclesListResponse {
	items := make([]map[string]any, 0, len(rows))
	maskSpec := mask.For(mask.ResourceVehicleSummary, role)
	for i := range rows {
		summary := newVehicleSummary(&rows[i], role, "")
		// `fieldsMasked` is intentionally discarded in v1: §7.0 reads
		// are not audited per `data-lifecycle.md` §4.2, and the v1
		// path only ever projects RoleOwner (which is the identity for
		// the owner allow-list — projection strips nothing). When the
		// viewer-merged invite-read pathway lands, this is where the
		// 1% sampled `mask_applied` audit hook (mirrors
		// `vehicle_status_mask.go` maybeEmitAuditREST) gets wired so
		// a misclassified field surfaces in the audit stream rather
		// than silently dropping.
		projected, _ := mask.Apply(summary.toMaskMap(), maskSpec)
		items = append(items, projected)
	}
	return vehiclesListResponse{Items: items}
}

// newVehicleSummary builds the per-row wire shape for a catalog row under a
// given role. tier is the caller's share permission and is stamped ONLY on
// viewer rows — pass the empty string for an owner.
//
// Shared by the owner list, the viewer merge (vehicles_list_viewer.go), and the
// redeem response, so all three emit byte-identical rows for the same vehicle.
func newVehicleSummary(v *VehicleCatalogRow, role auth.Role, tier auth.SharePermission) vehicleSummary {
	summary := vehicleSummary{
		VehicleID:      v.ID,
		Name:           v.Name,
		Model:          v.Model,
		Year:           v.Year,
		Color:          v.Color,
		LicensePlate:   v.LicensePlate,
		VinLast4:       lastFourOfVIN(v.VIN),
		Status:         v.Status,
		ChargeLevel:    v.ChargeLevel,
		EstimatedRange: v.EstimatedRange,
		LastUpdated:    v.LastUpdated.UTC().Format(time.RFC3339),
		Role:           string(role),
		HasActiveRide:  v.HasActiveRide,
		// MYR-316: resolved here so the precedence and the in-service gate
		// are applied exactly once per surface (service_window.go).
		ServiceEstimatedEndAt: serviceEstimatedEndAtWire(v.Status, v.ServiceETC, v.ServiceExpectedEndAt),
	}
	if role == auth.RoleViewer {
		summary.SharePermission = string(tier)
	}
	return summary
}

// lastFourOfVIN returns the last 4 characters of a VIN. Empty input
// yields an empty string; shorter-than-4 inputs are returned verbatim
// (the contract guarantees 17-char VINs, but the helper is defensive
// against test fixtures that pass shorter values).
func lastFourOfVIN(vin string) string {
	if len(vin) <= 4 {
		return vin
	}
	return vin[len(vin)-4:]
}

// writeJSON marshals v as JSON with the given status code.
func (h *VehiclesListHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("vehicles list: writeJSON encode failed", slog.String("error", err.Error()))
	}
}

// writeError writes the REST error envelope (rest-api.md §4.1) with a
// typed wserrors.ErrorCode. Mirrors vehicle_status_handler.go.
func (h *VehiclesListHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
