package push

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// The device-registry endpoints (rest-api.md §7.17):
//
//	PUT    /api/push/devices   register / refresh this installation's token
//	DELETE /api/push/devices   unregister on sign-out
//
// Both are user-scoped: there is no vehicle in the path and therefore no
// ownership check — the JWT subject IS the resource owner, like §7.7's
// /api/users/me surface.

// maxDeviceTokenLen bounds an accepted token. A real APNs token is 64 hex
// characters today, but Apple has changed the length before, so the check is a
// generous sanity bound rather than an exact format assertion — rejecting a
// valid future token would silently break push for everyone on that iOS
// release, which is far worse than storing a slightly odd string.
const maxDeviceTokenLen = 256

// tokenValidator validates a bearer JWT and returns the authenticated user id.
// Matches auth.JWTAuthenticator.ValidateToken; declared here (consumer site)
// so internal/push does not depend on the auth package.
type tokenValidator interface {
	ValidateToken(ctx context.Context, token string) (userID string, err error)
}

// DeviceRegistry is the persistence seam for the two endpoints. Satisfied by
// *store.PushDeviceRepo directly; no adapter needed.
type DeviceRegistry interface {
	RegisterDevice(ctx context.Context, userID, deviceToken string, sandbox bool) error
	UnregisterDevice(ctx context.Context, userID, deviceToken string) (bool, error)
}

// DevicesHandler serves the push device registry.
type DevicesHandler struct {
	auth     tokenValidator
	registry DeviceRegistry
	logger   *slog.Logger
}

// NewDevicesHandler wires the registry endpoints.
func NewDevicesHandler(auth tokenValidator, registry DeviceRegistry, logger *slog.Logger) *DevicesHandler {
	return &DevicesHandler{auth: auth, registry: registry, logger: logger}
}

// deviceRequest is the body of both endpoints: `{"deviceToken":"…","sandbox":true}`.
// DELETE ignores `sandbox` — a token is unregistered by identity, not flavour.
type deviceRequest struct {
	DeviceToken string `json:"deviceToken"`
	Sandbox     bool   `json:"sandbox"`
}

// registerResponse confirms a registration. It deliberately does NOT echo the
// device token: the token is P1 (data-classification.md §1.14) and echoing it
// would put it in every client log and proxy trace for no benefit — the caller
// already knows the value it sent.
type registerResponse struct {
	Registered bool `json:"registered"`
	Sandbox    bool `json:"sandbox"`
}

// unregisterResponse reports whether a registration was actually removed.
// `false` covers both "already gone" and "registered to somebody else",
// deliberately indistinguishable so the endpoint cannot probe other people's
// registrations.
type unregisterResponse struct {
	Unregistered bool `json:"unregistered"`
}

// ServeRegister handles PUT /api/push/devices.
func (h *DevicesHandler) ServeRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		h.writeError(w, http.StatusMethodNotAllowed, wserrors.ErrCodeInvalidRequest, "method not allowed")
		return
	}

	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}
	body, ok := h.decode(w, r)
	if !ok {
		return
	}

	if err := h.registry.RegisterDevice(r.Context(), userID, body.DeviceToken, body.Sandbox); err != nil {
		h.logger.Error("push devices: register failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	// P1: only the opaque user id, the token PREFIX, and the build flavour.
	h.logger.Info("push device registered",
		slog.String("user_id", userID),
		slog.String("device_token_prefix", tokenPrefix(body.DeviceToken)),
		slog.Bool("sandbox", body.Sandbox),
	)
	h.writeJSON(w, http.StatusOK, registerResponse{Registered: true, Sandbox: body.Sandbox})
}

// ServeUnregister handles DELETE /api/push/devices.
func (h *DevicesHandler) ServeUnregister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		h.writeError(w, http.StatusMethodNotAllowed, wserrors.ErrCodeInvalidRequest, "method not allowed")
		return
	}

	userID, ok := h.authUser(w, r)
	if !ok {
		return
	}
	body, ok := h.decode(w, r)
	if !ok {
		return
	}

	removed, err := h.registry.UnregisterDevice(r.Context(), userID, body.DeviceToken)
	if err != nil {
		h.logger.Error("push devices: unregister failed",
			slog.String("user_id", userID),
			slog.String("error", err.Error()),
		)
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	h.logger.Info("push device unregistered",
		slog.String("user_id", userID),
		slog.String("device_token_prefix", tokenPrefix(body.DeviceToken)),
		slog.Bool("removed", removed),
	)
	h.writeJSON(w, http.StatusOK, unregisterResponse{Unregistered: removed})
}

// decode parses and validates the shared request body.
func (h *DevicesHandler) decode(w http.ResponseWriter, r *http.Request) (deviceRequest, bool) {
	// Strict decode (unknown keys are a 400), matching the sibling handlers:
	// a typo'd key must fail loudly rather than silently registering a phone
	// with an empty token.
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	var body deviceRequest
	if err := dec.Decode(&body); err != nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "malformed request body")
		return deviceRequest{}, false
	}

	body.DeviceToken = strings.TrimSpace(body.DeviceToken)
	if msg := validateDeviceToken(body.DeviceToken); msg != "" {
		// The rejected value is P1 — describe the RULE, never echo the input.
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, msg)
		return deviceRequest{}, false
	}
	return body, true
}

// validateDeviceToken returns a rule description when the token is
// unacceptable, or "" when it is fine. It never quotes the value.
func validateDeviceToken(token string) string {
	switch {
	case token == "":
		return "deviceToken is required"
	case len(token) > maxDeviceTokenLen:
		return "deviceToken is too long"
	case strings.ContainsFunc(token, isTokenSeparator):
		return "deviceToken must not contain whitespace or control characters"
	default:
		return ""
	}
}

// isTokenSeparator reports whether r is whitespace or a control character —
// neither belongs in an APNs token and both indicate a client that has
// concatenated or mis-encoded the value.
func isTokenSeparator(r rune) bool {
	return r <= ' ' || r == 0x7f
}

// authUser resolves the bearer token to a user id, writing a 401 on failure.
func (h *DevicesHandler) authUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	token := bearerToken(r)
	if token == "" {
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "missing Authorization header")
		return "", false
	}
	userID, err := h.auth.ValidateToken(r.Context(), token)
	if err != nil {
		h.logger.Warn("push devices: invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return "", false
	}
	return userID, true
}

// bearerToken extracts the credential from an `Authorization: Bearer …` header.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, prefix) {
		return ""
	}
	return auth[len(prefix):]
}

// writeJSON / writeError mirror the sibling handlers (rest-api.md §4.1 typed
// error envelope).
func (h *DevicesHandler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("writeJSON: encode failed", slog.String("error", err.Error()))
	}
}

func (h *DevicesHandler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
