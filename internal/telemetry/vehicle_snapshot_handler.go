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

// VehicleSnapshotHandler handles GET /api/vehicles/{vehicleId}/snapshot.
// It validates the caller's JWT, verifies vehicle ownership, projects
// the response through the role-based VehicleState mask, and returns
// the canonical v1 VehicleState shape per
// docs/contracts/schemas/vehicle-state.schema.json.
//
// MYR-184 OPENED THIS ENDPOINT TO VIEWERS. It previously 403'd every
// non-owner, on the grounds that viewer access needed a table the Go server
// did not read. It now reads go_vehicle_shares: a caller holding an ACCEPTED
// share of at least the `live` tier — which is every tier, since the tiers are
// cumulative and `live` is the floor — gets a 200 under the VIEWER mask.
//
// P1 LOCATION IS VISIBLE TO VIEWERS, deliberately. Live location is the entire
// product being shared ("Live location" is the lowest tier's own name), so the
// viewer mask strips only the full `vin` (MYR-279). What stays owner-only is
// everything that MUTATES the car — commands, plate, service window, teardown,
// refresh — and those endpoints are untouched by this change.
type VehicleSnapshotHandler struct {
	auth     tokenValidator
	vehicles VehicleSnapshotReader
	roles    roleResolver // optional: nil disables role-based mask plumbing
	// shares resolves accepted share grants (MYR-184). Nil keeps the
	// pre-MYR-184 owner-only behaviour — the fail-closed direction.
	shares VehicleShareReader

	// Mask-audit fields (MYR-71, rest-api.md §5.3). All optional —
	// nil auditEmitter disables emit.
	auditEmitter  mask.AuditEmitter
	auditMetrics  mask.AuditMetrics
	auditEndpoint string

	logger *slog.Logger
}

// VehicleSnapshotOption configures optional dependencies on
// VehicleSnapshotHandler.
type VehicleSnapshotOption func(*VehicleSnapshotHandler)

// WithSnapshotRoleResolver enables role-based field masking on the
// handler. When omitted, the handler emits the unmasked response (the
// only path that reaches 200 in v1 is the owner, whose mask is the
// identity for the v1 owner allow-list).
func WithSnapshotRoleResolver(roles roleResolver) VehicleSnapshotOption {
	return func(h *VehicleSnapshotHandler) {
		h.roles = roles
	}
}

// WithSnapshotShareReader lets the handler admit VIEWERS holding an accepted
// vehicle share (MYR-184). Without it the handler is owner-only, which is the
// pre-MYR-184 behaviour and the fail-closed default.
func WithSnapshotShareReader(shares VehicleShareReader) VehicleSnapshotOption {
	return func(h *VehicleSnapshotHandler) {
		h.shares = shares
	}
}

// WithSnapshotMaskAudit attaches a mask-audit emitter to the handler
// (MYR-71, rest-api.md §5.3). When configured, every response whose
// mask projection removed at least one field is audit-logged at a 1%
// deterministic-hash sample rate. The endpoint argument is the route
// pattern written to metadata.endpoint —
// "/api/vehicles/{vehicleId}/snapshot" — rather than the substituted
// URL so a vehicleID does not appear twice (it is already on
// AuditEntry.TargetID). emitter MAY be nil — in which case this
// option is a no-op.
func WithSnapshotMaskAudit(emitter mask.AuditEmitter, metrics mask.AuditMetrics, endpoint string) VehicleSnapshotOption {
	return func(h *VehicleSnapshotHandler) {
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

// NewVehicleSnapshotHandler creates a handler that serves the
// GET /api/vehicles/{vehicleId}/snapshot endpoint.
func NewVehicleSnapshotHandler(
	tokens tokenValidator,
	vehicles VehicleSnapshotReader,
	logger *slog.Logger,
	opts ...VehicleSnapshotOption,
) *VehicleSnapshotHandler {
	h := &VehicleSnapshotHandler{
		auth:     tokens,
		vehicles: vehicles,
		logger:   logger,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// ServeHTTP handles GET /api/vehicles/{vehicleId}/snapshot.
func (h *VehicleSnapshotHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	vehicleID := r.PathValue("vehicleId")
	if vehicleID == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "missing vehicleId")
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
		h.logger.Warn("vehicle snapshot: invalid token",
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return
	}

	row, access, ok := h.loadSnapshot(ctx, w, vehicleID, userID)
	if !ok {
		return
	}

	resp := buildSnapshotResponse(row)
	h.writeMaskedResponse(r, w, vehicleID, userID, access, resp)
}

// loadSnapshot fetches the snapshot row and resolves the caller's access to it.
// Returns the row and the resolved access on success. On failure writes an HTTP
// error response and returns ok=false.
//
// The access check requires only PermissionLive — the floor tier — because the
// snapshot IS the live-location surface the lowest tier exists to grant. Higher
// tiers add history and rides, neither of which this endpoint serves.
func (h *VehicleSnapshotHandler) loadSnapshot(
	ctx context.Context,
	w http.ResponseWriter,
	vehicleID, userID string,
) (VehicleSnapshotRow, vehicleAccess, bool) {
	row, err := h.vehicles.GetByID(ctx, vehicleID)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			// Rest-api.md §4.1.1: 404 intentionally indistinguishable
			// from "filtered out by ownership" so the server never
			// leaks the existence of vehicles the caller cannot see.
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "vehicle not found")
			return VehicleSnapshotRow{}, vehicleAccess{}, false
		}
		h.logger.Error("vehicle snapshot: lookup failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return VehicleSnapshotRow{}, vehicleAccess{}, false
	}

	access, err := vehicleAccessFor(ctx, h.shares, userID, vehicleID, row.UserID, auth.PermissionLive)
	if err != nil {
		if errors.Is(err, errNoVehicleAccess) {
			denyVehicleAccess(w, h.logger, "vehicle snapshot", vehicleID, userID)
			return VehicleSnapshotRow{}, vehicleAccess{}, false
		}
		// A share-lookup outage is a 500, not a 403 — hiding an outage
		// behind a permissions error is how it goes uninvestigated.
		h.logger.Error("vehicle snapshot: access resolution failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return VehicleSnapshotRow{}, vehicleAccess{}, false
	}

	return row, access, true
}

// writeMaskedResponse projects the response through the role-based
// field mask before encoding. When no roleResolver is configured the
// response is encoded directly — equivalent to RoleOwner allow-all for
// v1 callers (the only non-owner path is 403'd by loadSnapshot).
//
// Audit emit (MYR-71, rest-api.md §5.3): when at least one field is
// stripped AND ShouldAuditREST samples in at 1%, the handler fires a
// non-blocking AuditEntry insert via mask.EmitAsync.
func (h *VehicleSnapshotHandler) writeMaskedResponse(
	r *http.Request,
	w http.ResponseWriter,
	vehicleID, userID string,
	access vehicleAccess,
	resp vehicleSnapshotResponse,
) {
	if h.roles == nil {
		if access.Role == auth.RoleOwner {
			// No role plumbing and the caller owns the car — emit
			// unmasked, which is the identity for the owner allow-list.
			h.writeJSON(w, http.StatusOK, resp)
			return
		}
		// A VIEWER without a role resolver must still be masked. Before
		// MYR-184 this branch was unreachable because viewers were 403'd
		// upstream; now that they are not, falling through to the unmasked
		// response would hand a viewer the owner-only full `vin`.
		projected, _ := mask.Apply(resp.toMaskMap(), mask.For(mask.ResourceVehicleState, access.Role))
		h.writeJSON(w, http.StatusOK, projected)
		return
	}

	role, err := h.roles.ResolveRole(r.Context(), userID, vehicleID)
	if err != nil {
		// Fail-closed at the contract layer (rest-api.md §5): an
		// unresolvable role yields the empty Role("") sentinel which
		// makes mask.For return deny-all. Surface this as a 500 so
		// the caller knows the request didn't succeed silently.
		h.logger.Error("vehicle snapshot: role resolution failed",
			slog.String("vehicle_id", vehicleID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	projected, fieldsMasked := mask.Apply(resp.toMaskMap(), mask.For(mask.ResourceVehicleState, role))
	h.maybeEmitAudit(r, userID, vehicleID, role, fieldsMasked)
	h.writeJSON(w, http.StatusOK, projected)
}

// maybeEmitAudit evaluates the REST audit-emit gate and fires the
// non-blocking AuditEntry insert when sampling allows. Mirrors
// vehicle_status_mask.go maybeEmitAuditREST.
func (h *VehicleSnapshotHandler) maybeEmitAudit(
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
		h.logger.Warn("vehicle snapshot: BuildEntry failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		return
	}
	mask.EmitAsync(r.Context(), h.auditEmitter, h.auditMetrics, h.logger, entry)
}

// writeJSON marshals v as JSON with the given status code.
func (h *VehicleSnapshotHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("vehicle snapshot: writeJSON encode failed", slog.String("error", err.Error()))
	}
}

// writeError writes the REST error envelope (rest-api.md §4.1) with a
// typed wserrors.ErrorCode.
func (h *VehicleSnapshotHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
