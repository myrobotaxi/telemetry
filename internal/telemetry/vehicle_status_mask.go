package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/auth"
	"github.com/tnando/my-robo-taxi-telemetry/internal/mask"
	"github.com/tnando/my-robo-taxi-telemetry/internal/wserrors"
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
// vehicleIDLookup, maskResource) are optional because not every wiring
// path has them yet — when nil, the handler emits the response without
// role-based projection (equivalent to RoleOwner allow-all for v1
// callers).
type VehicleStatusOption func(*VehicleStatusHandler)

// WithMask enables role-based field masking on the handler. The caller
// MUST pass an explicit `mask.ResourceType` so the choice of allow-list
// is conscious — the response struct's wire-shape must match the named
// resource's allow-list in `internal/mask/tables.go` or fields will be
// silently stripped.
//
// Today this handler emits a connectivity-probe response (`vin`,
// `connected`, `last_message_at`, ...) whose shape does NOT match
// `mask.ResourceVehicleState` (the canonical VehicleState shape).
// Wiring this option for the connectivity probe would silently deny
// almost every field even for owners. The option exists for the future
// `/api/vehicles/{vehicleID}/snapshot` endpoint (rest-api.md §5.2.1)
// which will reuse this handler's plumbing — `cmd/telemetry-server/
// main.go` deliberately does NOT pass this option in v1.
//
// Both the `roleResolver` and `vehicleIDLookup` MUST be supplied
// together — the resolver needs a vehicleID, and the only way to
// derive it from the path-parameter VIN is via the lookup.
func WithMask(resource mask.ResourceType, roles roleResolver, idLookup vehicleIDLookup) VehicleStatusOption {
	return func(h *VehicleStatusHandler) {
		h.maskResource = resource
		h.roles = roles
		h.idLookup = idLookup
	}
}

// WithMaskAudit attaches a mask-audit emitter and metrics to the
// handler (MYR-71, rest-api.md §5.3). When configured, every REST
// response whose mask projection removed at least one field is
// audit-logged at a 1% deterministic-hash sample rate. The
// `endpoint` argument is the route pattern written to
// metadata.endpoint — pass "/api/vehicles/{vehicleId}/snapshot"
// rather than the substituted URL so a vehicleID does not appear
// twice (it is already on AuditEntry.TargetID). emitter MAY be nil
// — in which case this option is a no-op.
func WithMaskAudit(emitter mask.AuditEmitter, metrics mask.AuditMetrics, endpoint string) VehicleStatusOption {
	return func(h *VehicleStatusHandler) {
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

// writeMaskedResponse projects the response struct through the
// role-based field mask before encoding. When the optional
// roleResolver / vehicleIDLookup pair is not configured, the response
// is encoded directly — equivalent to RoleOwner allow-all behavior for
// v1 callers (the only non-owner path is 403'd by verifyOwnership).
//
// Each maskable response struct provides a typed ToMaskMap() method
// that builds a wire-name-keyed map directly (no json.Marshal/Unmarshal
// round-trip). The mask matrix is keyed by JSON field name, so the
// helper hand-mirrors the struct's `json:"..."` tags — the same
// matrix-keyed design used by the WebSocket per-role projection
// (websocket-protocol.md §4.6).
//
// Audit emit (MYR-71, rest-api.md §5.3): when at least one field is
// stripped AND ShouldAuditREST samples in at 1%, the handler fires a
// non-blocking AuditEntry insert via mask.EmitAsync. The 'requestID'
// hash input is the X-Request-ID header per §4.4 (server generates
// one if the client did not). Failures log slog.Warn and increment
// audit_log_write_failures_total — they MUST NOT drop the response.
func (h *VehicleStatusHandler) writeMaskedResponse(
	ctx context.Context,
	r *http.Request,
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
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	// vehicleID is the audit row's TargetID. Resolved best-effort: if
	// the lookup fails here it would have failed in resolveCallerRole
	// above (which 500s), so this branch is reached only on success.
	vehicleID, _ := h.idLookup.GetVehicleIDByVIN(ctx, vin)

	projected, fieldsMasked := mask.Apply(resp.ToMaskMap(), mask.For(h.maskResource, role))

	h.maybeEmitAuditREST(r, userID, vehicleID, role, fieldsMasked)

	h.writeJSON(w, http.StatusOK, projected)
}

// maybeEmitAuditREST evaluates the REST audit-emit gate. Pulled out
// of writeMaskedResponse so the gate logic is testable in isolation
// and the hot path stays linear.
func (h *VehicleStatusHandler) maybeEmitAuditREST(
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
		// BuildEntry only fails on programmer errors. Log and skip;
		// the response itself still goes out.
		h.logger.Warn("vehicle status: BuildEntry failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		return
	}
	mask.EmitAsync(r.Context(), h.auditEmitter, h.auditMetrics, h.logger, entry)
}

// requestIDFromRequest returns the X-Request-ID header echoed by
// rest-api.md §4.4. If the client did not send one, a random ID is
// generated so ShouldAuditREST still has unique sampling input per
// request. The generated ID is NOT propagated back to the response —
// adding the response header is left to a future request-ID
// middleware (out of scope for MYR-71).
func requestIDFromRequest(r *http.Request) string {
	if v := r.Header.Get("X-Request-ID"); v != "" {
		return v
	}
	// Fall back to a server-generated random ID so the sampling
	// remains uniform across clients that omit the header.
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Crypto RNG unavailable is unrecoverable; degrade to a
		// timestamp-derived value rather than panic.
		return fmt.Sprintf("ts-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
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
