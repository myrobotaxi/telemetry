package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/mask"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// VehicleDrivesHandler handles GET /api/vehicles/{vehicleId}/drives.
// It validates the caller's JWT, verifies vehicle ownership, fetches a
// page of drives, projects each row through the role-based
// DriveSummary mask, and returns the paginated envelope per
// rest-api.md §7.2.
type VehicleDrivesHandler struct {
	auth     tokenValidator
	vehicles VehicleSnapshotReader // owner check via GetByID(vehicleId)
	drives   DriveLister
	roles    roleResolver // optional: nil disables role-based mask plumbing

	// Mask-audit fields (MYR-71, rest-api.md §5.3). All optional —
	// nil auditEmitter disables emit.
	auditEmitter  mask.AuditEmitter
	auditMetrics  mask.AuditMetrics
	auditEndpoint string

	logger *slog.Logger
}

// VehicleDrivesOption configures optional dependencies on
// VehicleDrivesHandler.
type VehicleDrivesOption func(*VehicleDrivesHandler)

// WithDrivesRoleResolver enables role-based field masking on the
// handler. Owners and viewers share the DriveSummary allow-list per
// rest-api.md §5.2.2, so in v1 this is plumbed for FR-5.5 readiness.
func WithDrivesRoleResolver(roles roleResolver) VehicleDrivesOption {
	return func(h *VehicleDrivesHandler) {
		h.roles = roles
	}
}

// WithDrivesMaskAudit attaches a mask-audit emitter to the handler
// (MYR-71, rest-api.md §5.3). The endpoint argument is the route
// pattern written to metadata.endpoint —
// "/api/vehicles/{vehicleId}/drives" — rather than the substituted
// URL. emitter MAY be nil — in which case this option is a no-op.
func WithDrivesMaskAudit(emitter mask.AuditEmitter, metrics mask.AuditMetrics, endpoint string) VehicleDrivesOption {
	return func(h *VehicleDrivesHandler) {
		if emitter == nil {
			return
		}
		h.auditEmitter = emitter
		if metrics == nil {
			metrics = mask.NoopAuditMetrics{}
		}
		h.auditMetrics = metrics
		h.auditEndpoint = endpoint
	}
}

// NewVehicleDrivesHandler creates a handler that serves the
// GET /api/vehicles/{vehicleId}/drives endpoint.
func NewVehicleDrivesHandler(
	tokens tokenValidator,
	vehicles VehicleSnapshotReader,
	drives DriveLister,
	logger *slog.Logger,
	opts ...VehicleDrivesOption,
) *VehicleDrivesHandler {
	h := &VehicleDrivesHandler{
		auth:     tokens,
		vehicles: vehicles,
		drives:   drives,
		logger:   logger,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// ServeHTTP handles GET /api/vehicles/{vehicleId}/drives.
func (h *VehicleDrivesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	vehicleID := r.PathValue("vehicleId")
	if vehicleID == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "missing vehicleId")
		return
	}

	limit, cursor, ok := h.parseQuery(w, r)
	if !ok {
		return
	}

	token := extractBearerToken(r)
	if token == "" {
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "missing Authorization header")
		return
	}

	ctx := r.Context()

	userID, err := h.auth.ValidateToken(ctx, token)
	if err != nil {
		h.logger.Warn("vehicle drives: invalid token",
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return
	}

	if !h.verifyOwnership(ctx, w, vehicleID, userID) {
		return
	}

	page, err := h.drives.ListByVehicleID(ctx, vehicleID, cursor, limit)
	if err != nil {
		h.logger.Error("vehicle drives: list failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	h.writeMaskedPage(r, w, vehicleID, userID, page)
}

// parseQuery validates and decodes the `limit` and `cursor` query
// params per rest-api.md §4.2.1. Returns ok=false after writing a
// 400 invalid_request envelope on any malformed input.
func (h *VehicleDrivesHandler) parseQuery(w http.ResponseWriter, r *http.Request) (int, DriveListCursor, bool) {
	limit := driveListDefaultLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < driveListMinLimit || n > driveListMaxLimit {
			h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "limit must be an integer in [1, 100]")
			return 0, DriveListCursor{}, false
		}
		limit = n
	}

	var cursor DriveListCursor
	if raw := r.URL.Query().Get("cursor"); raw != "" {
		c, err := decodeDriveCursor(raw)
		if err != nil {
			h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "malformed cursor")
			return 0, DriveListCursor{}, false
		}
		cursor = c
	}

	return limit, cursor, true
}

// verifyOwnership checks that userID owns the vehicle identified by
// vehicleID. Returns true if the check passes; on failure writes an
// HTTP error response and returns false. The 404 / 403 split mirrors
// the snapshot handler.
func (h *VehicleDrivesHandler) verifyOwnership(ctx context.Context, w http.ResponseWriter, vehicleID, userID string) bool {
	row, err := h.vehicles.GetByID(ctx, vehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "vehicle not found")
			return false
		}
		h.logger.Error("vehicle drives: vehicle lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return false
	}

	if row.UserID != userID {
		h.logger.Warn("vehicle drives: ownership mismatch",
			slog.String("vehicle_id", vehicleID),
			slog.String("user_id", userID),
		)
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodeVehicleNotOwned, "you do not own this vehicle")
		return false
	}

	return true
}

// writeMaskedPage projects each drive through the role mask, computes
// the nextCursor + hasMore wrapper, and writes the response envelope.
//
// When no roleResolver is configured the projection runs against
// auth.RoleOwner (the identity for the v1 driveSummary allow-list).
func (h *VehicleDrivesHandler) writeMaskedPage(
	r *http.Request,
	w http.ResponseWriter,
	vehicleID, userID string,
	page DriveListPage,
) {
	role := auth.RoleOwner
	if h.roles != nil {
		resolved, err := h.roles.ResolveRole(r.Context(), userID, vehicleID)
		if err != nil {
			h.logger.Error("vehicle drives: role resolution failed",
				slog.String("vehicle_id", vehicleID),
				slog.String("error", err.Error()),
			)
			h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
			return
		}
		role = resolved
	}

	maskSpec := mask.For(mask.ResourceDriveSummary, role)
	items := make([]map[string]any, 0, len(page.Items))
	var anyMasked []string
	for i := range page.Items {
		summary := buildDriveSummary(&page.Items[i])
		projected, fieldsMasked := mask.Apply(summary.toMaskMap(), maskSpec)
		items = append(items, projected)
		if anyMasked == nil && len(fieldsMasked) > 0 {
			anyMasked = fieldsMasked
		}
	}

	h.maybeEmitAudit(r, userID, vehicleID, role, anyMasked)

	var nextCursor *string
	if page.HasMore && len(page.Items) > 0 {
		last := page.Items[len(page.Items)-1]
		encoded := encodeDriveCursor(DriveListCursor{StartTime: last.StartTime, ID: last.ID})
		nextCursor = &encoded
	}

	h.writeJSON(w, http.StatusOK, drivesPageResponse{
		Items:      items,
		NextCursor: nextCursor,
		HasMore:    page.HasMore,
	})
}

// maybeEmitAudit evaluates the REST audit-emit gate for the page.
// Audit emits once per request (not once per item) per rest-api.md §5.3.
func (h *VehicleDrivesHandler) maybeEmitAudit(
	r *http.Request,
	userID, vehicleID string,
	role auth.Role,
	fieldsMasked []string,
) {
	if h.auditEmitter == nil {
		return
	}
	if len(fieldsMasked) == 0 {
		return
	}
	requestID := requestIDFromRequest(r)
	if !mask.ShouldAuditREST(userID, requestID, vehicleID) {
		return
	}

	entry, err := mask.BuildEntry(
		userID,
		mask.TargetRESTResponse,
		vehicleID,
		role,
		mask.AuditChannelREST,
		fieldsMasked,
		h.auditEndpoint,
	)
	if err != nil {
		h.logger.Warn("vehicle drives: BuildEntry failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		return
	}
	mask.EmitAsync(r.Context(), h.auditEmitter, h.auditMetrics, h.logger, entry)
}

// writeJSON marshals v as JSON with the given status code.
func (h *VehicleDrivesHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("vehicle drives: writeJSON encode failed", slog.String("error", err.Error()))
	}
}

// writeError writes the REST error envelope (rest-api.md §4.1) with a
// typed wserrors.ErrorCode.
func (h *VehicleDrivesHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
