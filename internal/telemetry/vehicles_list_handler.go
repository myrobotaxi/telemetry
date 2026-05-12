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
	ID             string
	VIN            string
	Name           string
	Model          string
	Year           int
	Color          string
	Status         string
	ChargeLevel    int
	EstimatedRange int
	LastUpdated    time.Time
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
// v1 only emits the owner pathway. Viewer-merged enumeration —
// merging in vehicles the caller has accepted-invite access to —
// requires the Go server to read the Prisma-owned Invite table and is
// tracked as a follow-up. Per rest-api.md §7.0, viewer-tier callers
// receive an empty list in v1.
type VehiclesListHandler struct {
	auth     tokenValidator
	vehicles VehicleLister
	logger   *slog.Logger
}

// NewVehiclesListHandler creates a handler that serves the
// GET /api/vehicles list endpoint.
func NewVehiclesListHandler(
	tokens tokenValidator,
	vehicles VehicleLister,
	logger *slog.Logger,
) *VehiclesListHandler {
	return &VehiclesListHandler{
		auth:     tokens,
		vehicles: vehicles,
		logger:   logger,
	}
}

// vehicleSummary is the per-row catalog shape returned by the list
// endpoint. JSON tags mirror the wire schema in rest-api.md §7.0 and
// `VehicleSummary` in specs/rest.openapi.yaml. See also the mask
// allow-list in `internal/mask/tables.go` (vehicleSummaryOwnerFields).
type vehicleSummary struct {
	VehicleID      string `json:"vehicleId"`
	Name           string `json:"name"`
	Model          string `json:"model"`
	Year           int    `json:"year"`
	Color          string `json:"color"`
	VinLast4       string `json:"vinLast4"`
	Status         string `json:"status"`
	ChargeLevel    int    `json:"chargeLevel"`
	EstimatedRange int    `json:"estimatedRange"`
	LastUpdated    string `json:"lastUpdated"`
	Role           string `json:"role"`
}

// toMaskMap returns the row as a wire-name-keyed map suitable for
// projection through the role-based mask. Mirrors the pattern in
// vehicle_status_handler.go ToMaskMap.
func (v vehicleSummary) toMaskMap() map[string]any {
	return map[string]any{
		"vehicleId":      v.VehicleID,
		"name":           v.Name,
		"model":          v.Model,
		"year":           v.Year,
		"color":          v.Color,
		"vinLast4":       v.VinLast4,
		"status":         v.Status,
		"chargeLevel":    v.ChargeLevel,
		"estimatedRange": v.EstimatedRange,
		"lastUpdated":    v.LastUpdated,
		"role":           v.Role,
	}
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

	// v1: ListByUser returns vehicles where Vehicle.userId == userID, so
	// every row is owned by the caller. Viewer-merged enumeration is
	// PLANNED — see rest-api.md §7.0 RBAC v1 implementation note.
	resp := h.buildResponse(rows, auth.RoleOwner)
	h.writeJSON(w, http.StatusOK, resp)
}

// buildResponse projects each Vehicle row through the per-role
// VehicleSummary mask and assembles the response envelope.
//
// The mask is applied per-row so a future viewer-merged implementation
// (where different rows have different roles) lands without rewriting
// the projection. In v1 every row gets the same RoleOwner mask, which
// is the identity for the v1 owner allow-list.
func (h *VehiclesListHandler) buildResponse(rows []VehicleCatalogRow, role auth.Role) vehiclesListResponse {
	items := make([]map[string]any, 0, len(rows))
	maskSpec := mask.For(mask.ResourceVehicleSummary, role)
	for i := range rows {
		v := &rows[i]
		summary := vehicleSummary{
			VehicleID:      v.ID,
			Name:           v.Name,
			Model:          v.Model,
			Year:           v.Year,
			Color:          v.Color,
			VinLast4:       lastFourOfVIN(v.VIN),
			Status:         v.Status,
			ChargeLevel:    v.ChargeLevel,
			EstimatedRange: v.EstimatedRange,
			LastUpdated:    v.LastUpdated.UTC().Format(time.RFC3339),
			Role:           string(role),
		}
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
