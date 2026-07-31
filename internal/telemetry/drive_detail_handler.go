package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/mask"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// DriveDetailHandler handles GET /api/drives/{driveId}. It validates the
// caller's JWT, looks up the drive (404 if unknown), verifies the caller
// owns the drive's vehicle, projects the response through the role-based
// DriveDetail mask, and returns the full FR-3.4 record (minus
// routePoints) per rest-api.md §7.3 / §5.2.3. Mirrors DriveRouteHandler's
// auth/ownership/mask flow.
type DriveDetailHandler struct {
	auth     tokenValidator
	vehicles VehicleSnapshotReader // owner check via GetByID(vehicleId)
	drives   DriveDetailFetcher
	roles    roleResolver // optional: nil disables role-based mask plumbing

	// Mask-audit fields (MYR-71, rest-api.md §5.3). All optional —
	// nil auditEmitter disables emit.
	auditEmitter  mask.AuditEmitter
	auditMetrics  mask.AuditMetrics
	auditEndpoint string

	logger *slog.Logger
}

// DriveDetailOption configures optional dependencies on
// DriveDetailHandler.
type DriveDetailOption func(*DriveDetailHandler)

// WithDriveDetailRoleResolver enables role-based field masking on the
// handler. Owners and viewers share the DriveDetail allow-list per
// rest-api.md §5.2.3, so this is plumbed for FR-5.1 sharing readiness —
// the mask is a no-op for both roles today.
func WithDriveDetailRoleResolver(roles roleResolver) DriveDetailOption {
	return func(h *DriveDetailHandler) {
		h.roles = roles
	}
}

// WithDriveDetailMaskAudit attaches a mask-audit emitter (MYR-71,
// rest-api.md §5.3). endpoint is the route pattern written to
// metadata.endpoint. emitter MAY be nil — in which case this is a no-op.
func WithDriveDetailMaskAudit(emitter mask.AuditEmitter, metrics mask.AuditMetrics, endpoint string) DriveDetailOption {
	return func(h *DriveDetailHandler) {
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

// NewDriveDetailHandler creates a handler that serves
// GET /api/drives/{driveId}.
func NewDriveDetailHandler(
	tokens tokenValidator,
	vehicles VehicleSnapshotReader,
	drives DriveDetailFetcher,
	logger *slog.Logger,
	opts ...DriveDetailOption,
) *DriveDetailHandler {
	h := &DriveDetailHandler{
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

// ServeHTTP handles GET /api/drives/{driveId}.
func (h *DriveDetailHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	driveID := r.PathValue("driveId")
	if driveID == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "missing driveId")
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
		h.logger.Warn("drive detail: invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return
	}

	// Fetch the drive first — we need its vehicleId to authorize. A
	// missing drive is a 404 before we reveal anything else.
	data, err := h.drives.GetDriveDetail(ctx, driveID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "drive not found")
			return
		}
		h.logger.Error("drive detail: fetch failed",
			slog.String("drive_id", driveID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	if !h.verifyOwnership(ctx, w, driveID, data.VehicleID, userID) {
		return
	}

	h.writeMaskedDetail(r, w, userID, data)
}

// verifyOwnership resolves the caller's access to the drive's vehicle: the
// OWNER, and nobody else (MYR-369 — no share of any shape opens the drives
// surfaces). Returns
// true on success; on failure writes an HTTP error and returns false.
// A drive that points at a missing vehicle is a data-integrity fault
// (500), distinct from an ownership mismatch (403, vehicle_not_owned).
func (h *DriveDetailHandler) verifyOwnership(ctx context.Context, w http.ResponseWriter, driveID, vehicleID, userID string) bool {
	row, err := h.vehicles.GetByID(ctx, vehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			// The drive resolved but its vehicle did not — inconsistent
			// data, not a client error.
			h.logger.Error("drive detail: drive's vehicle not found",
				slog.String("drive_id", driveID),
				slog.String("vehicle_id", vehicleID),
			)
			h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
			return false
		}
		h.logger.Error("drive detail: vehicle lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return false
	}

	// MYR-369: THE DRIVES SURFACES ARE OWNER-ONLY AGAIN, unconditionally.
	//
	// MYR-184 opened them to a viewer holding `live_history` or better. That
	// tier is RETIRED and the capability is removed from the product, so the
	// gate is back to what it was before sharing shipped: owner or nobody.
	// This is a DELIBERATE NARROWING — a legacy grant created at
	// `live_history` can no longer read drives, and there is no flag that
	// re-opens them. Suspension is irrelevant here for the same reason: no
	// grant of any shape passes.
	//
	// Expressed as capBase-against-the-owner rather than a bare
	// `row.UserID != userID` comparison so the denial still flows through the
	// one access helper — same 403, same log shape, same non-oracle message
	// as every other refusal — and so re-opening the surface later is a
	// one-argument change rather than a re-derivation.
	if _, err := vehicleAccessForOwnerOnly(ctx, userID, row.UserID); err != nil {
		if errors.Is(err, errNoVehicleAccess) {
			denyVehicleAccess(w, h.logger, "drive detail", vehicleID, userID)
			return false
		}
		h.logger.Error("drive detail: access resolution failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return false
	}

	return true
}

// writeMaskedDetail resolves the caller's role, projects the detail
// through the DriveDetail mask, and writes the response. When no
// roleResolver is configured the projection runs against auth.RoleOwner.
func (h *DriveDetailHandler) writeMaskedDetail(r *http.Request, w http.ResponseWriter, userID string, data DriveDetailData) {
	role := auth.RoleOwner
	if h.roles != nil {
		resolved, err := h.roles.ResolveRole(r.Context(), userID, data.VehicleID)
		if err != nil {
			h.logger.Error("drive detail: role resolution failed",
				slog.String("vehicle_id", data.VehicleID),
				slog.String("error", err.Error()),
			)
			h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
			return
		}
		role = resolved
	}

	maskSpec := mask.For(mask.ResourceDriveDetail, role)
	projected, fieldsMasked := mask.Apply(buildDriveDetail(data).toMaskMap(), maskSpec)

	h.maybeEmitAudit(r, userID, data.VehicleID, role, fieldsMasked)

	h.writeJSON(w, http.StatusOK, projected)
}

// maybeEmitAudit evaluates the REST audit-emit gate. Audit emits once
// per request per rest-api.md §5.3. For DriveDetail both roles see the
// same field set, so this never fires today — it's plumbed for when
// sharing/viewer masking lands (FR-5.1), mirroring the route handler.
func (h *DriveDetailHandler) maybeEmitAudit(r *http.Request, userID, vehicleID string, role auth.Role, fieldsMasked []string) {
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
		h.logger.Warn("drive detail: BuildEntry failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		return
	}
	mask.EmitAsync(r.Context(), h.auditEmitter, h.auditMetrics, h.logger, entry)
}

// writeJSON marshals v as JSON with the given status code.
func (h *DriveDetailHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("drive detail: writeJSON encode failed", slog.String("error", err.Error()))
	}
}

// writeError writes the REST error envelope (rest-api.md §4.1).
func (h *DriveDetailHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
