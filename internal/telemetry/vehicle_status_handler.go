package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/mask"
	"github.com/tnando/my-robo-taxi-telemetry/internal/wserrors"
	"github.com/tnando/my-robo-taxi-telemetry/pkg/sdk"
)

// VehiclePresence provides information about which vehicles are currently
// connected to the telemetry receiver.
type VehiclePresence interface {
	ConnectionInfo(vin string) (ConnInfo, bool)
}

// ConnInfo describes an active vehicle connection.
type ConnInfo struct {
	ConnectedSince time.Time
	LastMessageAt  time.Time
	MessageCount   int64
}

// VehicleStatusHandler handles GET /api/vehicle-status/{vin} requests. It
// validates the caller's JWT, verifies vehicle ownership, and returns the
// vehicle's current connection status. The frontend polls this endpoint
// during onboarding to detect when a vehicle connects.
type VehicleStatusHandler struct {
	auth         tokenValidator
	vehicles     VehicleOwnerLookup
	roles        roleResolver      // optional: nil disables mask plumbing
	idLookup     vehicleIDLookup   // optional: nil disables mask plumbing
	maskResource mask.ResourceType // populated by WithMask alongside roles/idLookup

	// Mask-audit fields (MYR-71, rest-api.md §5.3). All optional —
	// nil auditEmitter disables emit. WithMaskAudit fills them.
	auditEmitter  mask.AuditEmitter
	auditMetrics  mask.AuditMetrics
	auditEndpoint string // route pattern for metadata.endpoint, e.g. "/api/vehicles/{vehicleId}/snapshot"

	presence VehiclePresence
	logger   *slog.Logger
}

// NewVehicleStatusHandler creates a handler that returns real-time vehicle
// connection status. The presence provider is typically the telemetry Receiver.
// Pass WithMask to enable field-mask projection of the response.
func NewVehicleStatusHandler(
	tokens tokenValidator,
	vehicles VehicleOwnerLookup,
	presence VehiclePresence,
	logger *slog.Logger,
	opts ...VehicleStatusOption,
) *VehicleStatusHandler {
	h := &VehicleStatusHandler{
		auth:     tokens,
		vehicles: vehicles,
		presence: presence,
		logger:   logger,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h
}

// vehicleStatusResponse is the JSON body returned by the vehicle status endpoint.
type vehicleStatusResponse struct {
	VIN            string  `json:"vin"`
	Connected      bool    `json:"connected"`
	LastMessageAt  *string `json:"last_message_at"`
	MessageCount   int64   `json:"message_count"`
	ConnectedSince *string `json:"connected_since"`
}

// ToMaskMap returns a map[string]any keyed by JSON wire name, suitable
// for projection through the role-based field mask in internal/mask.
// Replaces the encoding/json round-trip previously used by
// writeMaskedResponse, removing one Marshal/Unmarshal allocation pair
// per masked REST response (MYR-58).
//
// Pointer-typed fields (LastMessageAt, ConnectedSince) are flattened to
// their pointed-to value or nil — preserving the same key set the JSON
// round-trip produced. The mask matrix only inspects keys, so value
// type fidelity (int64 vs float64) is irrelevant; we use native Go
// types here.
func (r vehicleStatusResponse) ToMaskMap() map[string]any {
	m := make(map[string]any, 5)
	m["vin"] = r.VIN
	m["connected"] = r.Connected
	m["last_message_at"] = derefOrNil(r.LastMessageAt)
	m["message_count"] = r.MessageCount
	m["connected_since"] = derefOrNil(r.ConnectedSince)
	return m
}

// derefOrNil returns *p as an any, or untyped nil if p is nil. The
// untyped-nil return is what json.Marshal of a nil pointer produces
// after the Unmarshal-to-map round-trip — keeping the post-mask JSON
// output byte-identical to the pre-MYR-58 implementation.
func derefOrNil[T any](p *T) any {
	if p == nil {
		return nil
	}
	return *p
}

// ServeHTTP handles the vehicle status request.
func (h *VehicleStatusHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	vin := r.PathValue("vin")
	if len(vin) != vinLength {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "invalid VIN: must be 17 characters")
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
		h.logger.Warn("vehicle status: invalid token",
			slog.String("vin", redactVIN(vin)),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return
	}

	if !h.verifyOwnership(ctx, w, vin, userID) {
		return
	}

	resp := h.buildStatusResponse(vin)
	h.writeMaskedResponse(ctx, r, w, vin, userID, resp)
}

// verifyOwnership checks that userID owns the vehicle identified by vin.
// Returns true if the ownership check passes. On failure it writes an HTTP
// error response and returns false.
func (h *VehicleStatusHandler) verifyOwnership(ctx context.Context, w http.ResponseWriter, vin, userID string) bool {
	ownerID, err := h.vehicles.GetVehicleOwner(ctx, vin)
	if err != nil {
		if errors.Is(err, sdk.ErrNotFound) {
			h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "vehicle not found")
			return false
		}
		h.logger.Error("vehicle status: vehicle lookup failed",
			slog.String("vin", redactVIN(vin)),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return false
	}

	if ownerID != userID {
		h.logger.Warn("vehicle status: ownership mismatch",
			slog.String("vin", redactVIN(vin)),
			slog.String("user_id", userID),
		)
		h.writeError(w, http.StatusForbidden, wserrors.ErrCodeVehicleNotOwned, "you do not own this vehicle")
		return false
	}

	return true
}

// buildStatusResponse queries the presence provider and constructs the
// JSON response. Times are formatted as RFC 3339 or null when unavailable.
func (h *VehicleStatusHandler) buildStatusResponse(vin string) vehicleStatusResponse {
	resp := vehicleStatusResponse{
		VIN: redactVIN(vin),
	}

	info, connected := h.presence.ConnectionInfo(vin)
	if !connected {
		return resp
	}

	resp.Connected = true
	resp.MessageCount = info.MessageCount

	since := info.ConnectedSince.Format(time.RFC3339)
	resp.ConnectedSince = &since

	if !info.LastMessageAt.IsZero() {
		last := info.LastMessageAt.Format(time.RFC3339)
		resp.LastMessageAt = &last
	}

	return resp
}

// writeJSON marshals v as JSON and writes it with the given status code.
func (h *VehicleStatusHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("writeJSON: encode failed", slog.String("error", err.Error()))
	}
}

// writeError writes the REST error envelope (rest-api.md §4.1) with a
// typed wserrors.ErrorCode. Compiler-enforced no-string-literal-at-call-site.
func (h *VehicleStatusHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
