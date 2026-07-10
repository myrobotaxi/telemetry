package identity

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// Handler serves the /api/auth/* REST surface. It follows the repo's
// per-handler pattern (no shared auth middleware): strict JSON decoding,
// per-IP pre-auth rate limiting, and typed error envelopes via wserrors.
type Handler struct {
	svc      *Service
	keystore *Keystore
	limiter  *ipRateLimiter
	logger   *slog.Logger
	// appleEnabled is false when no Apple client id is configured — the sign
	// -in endpoint then 404s so it is not a half-configured foot-gun.
	appleEnabled bool
}

// HandlerConfig bundles the auth handler's construction inputs.
type HandlerConfig struct {
	Service      *Service
	Keystore     *Keystore
	AppleEnabled bool
	// RateLimitPerMinute / RateLimitBurst configure the per-IP pre-auth cap.
	RateLimitPerMinute int
	RateLimitBurst     int
	Logger             *slog.Logger
}

// NewHandler builds the auth HTTP handler, constructing its per-IP rate
// limiter internally.
func NewHandler(cfg HandlerConfig) *Handler {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		svc:          cfg.Service,
		keystore:     cfg.Keystore,
		limiter:      newIPRateLimiter(cfg.RateLimitPerMinute, cfg.RateLimitBurst),
		appleEnabled: cfg.AppleEnabled,
		logger:       logger,
	}
}

type appleSignInRequest struct {
	IdentityToken string `json:"identityToken"`
	FullName      string `json:"fullName,omitempty"`
	Email         string `json:"email,omitempty"`
	Nonce         string `json:"nonce,omitempty"`
}

type refreshRequest struct {
	RefreshToken string `json:"refreshToken"`
}

type revokeRequest struct {
	RefreshToken string `json:"refreshToken"`
}

type authResponse struct {
	AccessToken  string   `json:"accessToken"`
	ExpiresIn    int      `json:"expiresIn"`
	RefreshToken string   `json:"refreshToken"`
	User         UserInfo `json:"user"`
}

// ServeApple handles POST /api/auth/apple.
func (h *Handler) ServeApple(w http.ResponseWriter, r *http.Request) {
	if !h.appleEnabled {
		h.writeError(w, http.StatusNotFound, wserrors.ErrCodeNotFound, "sign in with apple is not enabled")
		return
	}
	if !h.allow(w, r) {
		return
	}
	var body appleSignInRequest
	if !decodeJSON(w, r, &body, h) {
		return
	}
	if strings.TrimSpace(body.IdentityToken) == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "identityToken is required")
		return
	}

	res, err := h.svc.SignInWithApple(r.Context(), AppleSignInInput(body))
	if err != nil {
		h.mapAuthError(w, "apple sign-in", err)
		return
	}
	h.writeJSON(w, http.StatusOK, toResponse(res))
}

// ServeRefresh handles POST /api/auth/refresh.
func (h *Handler) ServeRefresh(w http.ResponseWriter, r *http.Request) {
	if !h.allow(w, r) {
		return
	}
	var body refreshRequest
	if !decodeJSON(w, r, &body, h) {
		return
	}
	if strings.TrimSpace(body.RefreshToken) == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "refreshToken is required")
		return
	}
	res, err := h.svc.Refresh(r.Context(), body.RefreshToken)
	if err != nil {
		h.mapAuthError(w, "refresh", err)
		return
	}
	h.writeJSON(w, http.StatusOK, toResponse(res))
}

// ServeRevoke handles POST /api/auth/revoke. Always 204 on a well-formed
// request (no existence oracle) unless the store fails.
func (h *Handler) ServeRevoke(w http.ResponseWriter, r *http.Request) {
	if !h.allow(w, r) {
		return
	}
	var body revokeRequest
	if !decodeJSON(w, r, &body, h) {
		return
	}
	if strings.TrimSpace(body.RefreshToken) == "" {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "refreshToken is required")
		return
	}
	if err := h.svc.Revoke(r.Context(), body.RefreshToken); err != nil {
		h.logger.Error("revoke failed", slog.String("error", err.Error()))
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ServeJWKS handles GET /api/auth/.well-known/jwks.json — the public keys.
func (h *Handler) ServeJWKS(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	if err := json.NewEncoder(w).Encode(h.keystore.JWKS()); err != nil {
		h.logger.Error("jwks encode failed", slog.String("error", err.Error()))
	}
}

// allow enforces the per-IP rate limit; on breach it writes 429 and returns
// false.
func (h *Handler) allow(w http.ResponseWriter, r *http.Request) bool {
	if h.limiter == nil || h.limiter.allow(clientIP(r)) {
		return true
	}
	w.Header().Set("Retry-After", "1")
	h.writeError(w, http.StatusTooManyRequests, wserrors.ErrCodeRateLimited, "too many requests")
	return false
}

// mapAuthError maps a service error to a typed REST envelope. Auth failures
// collapse to 401 auth_failed with a generic message (no reuse/linkage
// oracle, no PII); everything else is a 500.
func (h *Handler) mapAuthError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, ErrInvalidAppleToken), errors.Is(err, ErrEmailNotVerified),
		errors.Is(err, ErrInvalidRefreshToken), errors.Is(err, ErrRefreshReuseDetected):
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "authentication failed")
	default:
		h.logger.Error(op+" failed", slog.String("error", err.Error()))
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
	}
}

func toResponse(res AuthResult) authResponse {
	return authResponse(res)
}

// decodeJSON strictly decodes the request body (unknown fields -> 400).
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, h *Handler) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		h.writeError(w, http.StatusBadRequest, wserrors.ErrCodeInvalidRequest, "malformed request body")
		return false
	}
	return true
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("writeJSON encode failed", slog.String("error", err.Error()))
	}
}

func (h *Handler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}

// clientIP resolves the caller IP for rate-limit keying: the leftmost
// X-Forwarded-For entry (the edge appends), else RemoteAddr's host.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first := strings.TrimSpace(strings.SplitN(xff, ",", 2)[0]); first != "" {
			return first
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
