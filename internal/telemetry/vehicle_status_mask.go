package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/tnando/my-robo-taxi-telemetry/internal/auth"
	"github.com/tnando/my-robo-taxi-telemetry/internal/mask"
)

// roleResolver returns the caller's role for a given vehicle. The
// vehicle-status endpoint plumbs role-resolution into the response
// path so the field-mask layer in internal/mask can project the body
// according to rest-api.md §5.2.1. v1 only owners reach a 200 here
// (viewers fall through verifyOwnership), so the mask is plumbed for
// FR-5.5 readiness rather than to strip fields today.
type roleResolver interface {
	ResolveRole(ctx context.Context, userID, vehicleID string) (auth.Role, error)
}

// vehicleIDLookup resolves a VIN to its DB primary key (cuid). Stays
// local to this package — the canonical mapping lives in store.VINCache.
type vehicleIDLookup interface {
	GetVehicleIDByVIN(ctx context.Context, vin string) (vehicleID string, err error)
}

// VehicleStatusOption configures optional dependencies on
// VehicleStatusHandler. The mask-plumbing dependencies (roleResolver,
// vehicleIDLookup) are optional because not every wiring path has them
// yet — when nil, the handler emits the response without role-based
// projection (equivalent to RoleOwner allow-all for v1 callers).
type VehicleStatusOption func(*VehicleStatusHandler)

// WithRoleResolver enables role-based field masking. Both the
// roleResolver and vehicleIDLookup MUST be supplied together — the
// resolver needs a vehicleID, and the only way to derive it from the
// path-parameter VIN is via the lookup.
func WithRoleResolver(roles roleResolver, idLookup vehicleIDLookup) VehicleStatusOption {
	return func(h *VehicleStatusHandler) {
		h.roles = roles
		h.idLookup = idLookup
	}
}

// writeMaskedResponse projects the response struct through the
// role-based field mask before encoding. When the optional
// roleResolver / vehicleIDLookup pair is not configured, the response
// is encoded directly — equivalent to RoleOwner allow-all behavior for
// v1 callers (the only non-owner path is 403'd by verifyOwnership).
//
// The map round-trip through json.Marshal/Unmarshal is intentional: the
// mask matrix is keyed by JSON field name, so we materialize the
// struct's wire shape as a map[string]any and project that map. The
// trade-off is one extra allocation per request; the matrix-keyed
// design is the same one used by the WebSocket per-role projection
// (websocket-protocol.md §4.6) — keeping both transports on a single
// source-of-truth is worth the marginal cost.
//
// TODO(MYR-XX audit-log): when AuditLog table exists, if
// len(fieldsMasked) > 0 AND mask.ShouldAuditREST(userID, requestID,
// vehicleID) == true, emit an audit entry per rest-api.md §5.3. The
// AuditLog migration is deferred (data-lifecycle.md §4 schema doesn't
// exist in Prisma yet — same cross-repo pattern as MYR-41's
// chargeState/timeToFull migration).
func (h *VehicleStatusHandler) writeMaskedResponse(
	ctx context.Context,
	w http.ResponseWriter,
	vin, userID string,
	resp vehicleStatusResponse,
) {
	// Mask plumbing not configured — emit raw response. v1's only
	// caller path here is the owner (verifyOwnership 403s the rest),
	// so the unmasked output matches the masked output for owners.
	if h.roles == nil || h.idLookup == nil {
		h.writeJSON(w, http.StatusOK, resp)
		return
	}

	role, err := h.resolveCallerRole(ctx, vin, userID)
	if err != nil {
		// Fail-closed at the contract layer (rest-api.md §5): an
		// unresolvable role yields the empty Role("") sentinel, which
		// makes mask.For return deny-all and produces an empty body.
		// Surface this as a 500 so the caller knows the request
		// didn't succeed silently.
		h.logger.Error("vehicle status: role resolution failed",
			slog.String("vin", redactVIN(vin)),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Round-trip through JSON to materialize the response as a
	// schema-less map[string]any keyed by wire field name. The mask
	// matrix is keyed the same way — see internal/mask/tables.go.
	asMap, err := structToMap(resp)
	if err != nil {
		h.logger.Error("vehicle status: encode-for-mask failed",
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	projected, fieldsMasked := mask.Apply(asMap, mask.For(mask.ResourceVehicleState, role))
	_ = fieldsMasked // see TODO(MYR-XX audit-log) above

	h.writeJSON(w, http.StatusOK, projected)
}

// resolveCallerRole derives the caller's role for the vehicle
// identified by VIN. The VIN is converted to vehicleID via the
// configured idLookup, then ResolveRole is queried.
func (h *VehicleStatusHandler) resolveCallerRole(ctx context.Context, vin, userID string) (auth.Role, error) {
	vehicleID, err := h.idLookup.GetVehicleIDByVIN(ctx, vin)
	if err != nil {
		return auth.Role(""), fmt.Errorf("resolveCallerRole: lookup vehicleID for vin: %w", err)
	}
	role, err := h.roles.ResolveRole(ctx, userID, vehicleID)
	if err != nil {
		return auth.Role(""), fmt.Errorf("resolveCallerRole: %w", err)
	}
	return role, nil
}

// structToMap encodes v as JSON and decodes it back into a
// map[string]any, producing a wire-name-keyed map suitable for the
// mask layer.
func structToMap(v any) (map[string]any, error) {
	encoded, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("structToMap: marshal: %w", err)
	}
	out := make(map[string]any)
	if err := json.Unmarshal(encoded, &out); err != nil {
		return nil, fmt.Errorf("structToMap: unmarshal: %w", err)
	}
	return out, nil
}
