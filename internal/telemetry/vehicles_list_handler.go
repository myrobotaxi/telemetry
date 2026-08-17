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

	// RideShareEnabled is the owner's ride-sharing switch (MYR-342), LEFT
	// JOINed from the same control-state row as the service window. Unlike the
	// two above it is emitted RAW — there is nothing to resolve, no precedence
	// and no status gate: the owner's answer is the wire value. The store's
	// COALESCE makes a car with no side-table row read true, so the zero value
	// this struct can hold on a hand-built row means PAUSED; every real
	// construction site goes through the adapter and carries the joined value.
	RideShareEnabled bool

	// TrimLabel is the DISPLAY-SAFE trim label (MYR-507), LEFT JOINed from the
	// same control-state row as the two above. Emitted RAW like
	// RideShareEnabled — there is nothing to resolve and no status gate — and it
	// is the SAME COLUMN /snapshot emits as VehicleState.trimLabel (MYR-320),
	// read rather than re-derived, so the catalog and the detail sheet cannot
	// name the same car two different things.
	//
	// Nil means Tesla has not told us the trim yet, and that is NOT the empty
	// string: a descriptor built from it must drop the fragment entirely rather
	// than render a stray separator.
	//
	// MYR-578: no longer emitted verbatim — `newVehicleSummary` resolves the
	// wire value through `internal/trim` (label → badge → VIN drive-unit), so a
	// junk stored label ("Base2024") or an absent one no longer decides what a
	// car is called when the badge or the VIN knows better.
	TrimLabel *string

	// Trim is the RAW BADGE CODE ("p100d"), MYR-578's second resolver rung.
	// NEVER emitted on this surface — input only.
	Trim *string

	// OwnerFirstName is the FIRST NAME of the person who owns this car
	// (MYR-581), resolved by the store's three-source identity ladder and
	// already reduced to its first token before it crosses this boundary — the
	// full name never reaches this package.
	//
	// Emitted RAW (there is nothing left to resolve), as
	// `VehicleSummary.ownerFirstName`. Nil means the owner has no resolvable
	// name in any identity source, which is a real and common state and NOT the
	// empty string: a descriptor built from it must drop the whole possessive
	// fragment rather than render "'s Model X".
	//
	// P1. NEVER logged.
	OwnerFirstName *string

	// Latitude / Longitude are the car's freshest known position (MYR-515),
	// already decrypted by the store from the SAME `latitudeEnc`/`longitudeEnc`
	// pair the /snapshot serves. An ATOMIC PAIR: both set or both nil.
	//
	// RAW, in the sense that no privacy transform is applied — but NOT emitted
	// verbatim: newVehicleSummary collapses the (0, 0) no-fix sentinel to a null
	// `location`, because a catalog row must never hand a picker a coordinate in
	// the Gulf of Guinea to measure an ETA against.
	Latitude  *float64
	Longitude *float64

	// SetupSchedule is the car's fleet-config schedule row (MYR-491), LEFT
	// JOINed from go_fleet_config_attempts. RAW, like the service-window pair:
	// the wire field `setupState` is DERIVED from it together with Status and
	// LastUpdated. The catalog carries it so the rider-side picker can render a
	// shared car as "setting up" instead of "offline" (MYR-437) without a
	// snapshot fetch per row. Zero value means "no claim" — the safe reading.
	SetupSchedule VehicleSetupSchedule
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
	// members supplies the MYR-540 group-ride member merge. Nil leaves the
	// endpoint owner+shared (see vehicles_list_member.go).
	members RideMemberVehicleLister
	logger  *slog.Logger
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
	// MYR-540: the vehicles of live group rides the caller JOINED, viewer
	// rows, deduplicated against the two halves above (vehicles_list_member.go).
	resp = h.appendMemberRows(ctx, userID, resp)
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
	// ONE reading of the clock for the whole page: the MYR-491 derivation is a
	// set of comparisons against it, and two rows of the same response must not
	// be judged against two different instants.
	now := time.Now()
	for i := range rows {
		summary := newVehicleSummary(&rows[i], role, auth.ShareGrant{}, now)
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
