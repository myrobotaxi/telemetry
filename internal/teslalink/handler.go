package teslalink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/myrobotaxi/telemetry/internal/teslaauth"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// ErrAccountNotProvisioned signals that the calling user has no Tesla Account
// row to write tokens into. The cmd-layer AccountLinker adapter maps
// store.ErrTeslaTokenNotFound to this so this package stays free of an
// internal/store dependency. See docs/architecture/owner-onboarding.md for the
// MVP provisioning step that must precede an in-app link.
var ErrAccountNotProvisioned = errors.New("tesla account not provisioned for user")

// tokenValidator validates a bearer access token and returns the user id
// (the JWT `sub`). Satisfied by *auth.JWTAuthenticator.
type tokenValidator interface {
	ValidateToken(ctx context.Context, token string) (string, error)
}

// AccountLinker persists a freshly linked Tesla token set for a user via the
// existing encrypted Account dual-write path (store.AccountRepo.UpdateTeslaToken).
// It returns ErrAccountNotProvisioned when the user has no Tesla Account row.
type AccountLinker interface {
	UpdateTeslaToken(ctx context.Context, userID, accessToken, refreshToken string, expiresAt int64) error
}

// exchangeFunc is the code->token seam, defaulted to teslaauth.ExchangeCodeForToken
// and overridable in tests so the callback path can be exercised without a live
// Tesla endpoint.
type exchangeFunc func(ctx context.Context, logger *slog.Logger, clientID, clientSecret, redirectURI, code, codeVerifier string) (*teslaauth.TokenResponse, error)

// Config bundles the handler's static construction inputs. ClientID/ClientSecret
// are the Tesla Fleet app credentials (AUTH_TESLA_ID/SECRET). RedirectURI is the
// backend callback URL registered on the Tesla app. AppRedirectURL is the app
// deep link the callback hands off to (e.g. myrobotaxi://tesla-linked). Scopes
// defaults to teslaauth.FullScopes when empty.
type Config struct {
	ClientID       string
	ClientSecret   string
	RedirectURI    string
	AppRedirectURL string
	Scopes         string
}

// Handler serves the two link endpoints. It follows the repo's per-handler
// pattern (no shared auth middleware): /start validates the bearer itself;
// /callback is unauthenticated (Tesla redirects a browser to it) and instead
// recovers the user from the single-use state session.
type Handler struct {
	auth     tokenValidator
	linker   AccountLinker
	sessions *SessionStore
	cfg      Config
	exchange exchangeFunc
	logger   *slog.Logger
	now      func() time.Time
}

// NewHandler builds the link handler. Scopes falls back to teslaauth.FullScopes
// when cfg.Scopes is empty.
func NewHandler(auth tokenValidator, linker AccountLinker, sessions *SessionStore, cfg Config, logger *slog.Logger) *Handler {
	if cfg.Scopes == "" {
		cfg.Scopes = teslaauth.FullScopes
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{
		auth:     auth,
		linker:   linker,
		sessions: sessions,
		cfg:      cfg,
		exchange: teslaauth.ExchangeCodeForToken,
		logger:   logger,
		now:      time.Now,
	}
}

// startResponse is the JSON body returned by /start. The client opens
// AuthorizeURL in ASWebAuthenticationSession; State is echoed for optional
// client-side correlation (the server is the authority on state validation).
type startResponse struct {
	AuthorizeURL string `json:"authorizeUrl"`
	State        string `json:"state"`
}

// ServeStart handles POST /api/tesla/link/start. It authenticates the caller,
// mints a PKCE+state session bound to the user, and returns the Tesla
// authorize URL (full scope set + prompt_missing_scopes=true).
func (h *Handler) ServeStart(w http.ResponseWriter, r *http.Request) {
	token := bearerToken(r)
	if token == "" {
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "missing Authorization header")
		return
	}
	userID, err := h.auth.ValidateToken(r.Context(), token)
	if err != nil {
		h.logger.Warn("tesla link start: invalid token", slog.String("error", err.Error()))
		h.writeError(w, http.StatusUnauthorized, wserrors.ErrCodeAuthFailed, "invalid or expired token")
		return
	}

	pkce, err := teslaauth.GeneratePKCE()
	if err != nil {
		h.logger.Error("tesla link start: pkce", slog.String("error", err.Error()))
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}
	state, err := teslaauth.RandomURLSafeString(32)
	if err != nil {
		h.logger.Error("tesla link start: state", slog.String("error", err.Error()))
		h.writeError(w, http.StatusInternalServerError, wserrors.ErrCodeInternalError, "internal error")
		return
	}

	h.sessions.Put(Session{State: state, PKCEVerifier: pkce.Verifier, UserID: userID})

	authorizeURL := teslaauth.BuildAuthorizeURL(h.cfg.ClientID, h.cfg.RedirectURI, h.cfg.Scopes, state, pkce.Challenge)
	h.logger.Info("tesla link started", slog.String("user_id", userID))
	h.writeJSON(w, http.StatusOK, startResponse{AuthorizeURL: authorizeURL, State: state})
}

// ServeCallback handles GET /api/tesla/link/callback. Tesla redirects the
// user's browser here after consent. It resolves the outcome and redirects the
// browser to the app deep link (myrobotaxi://tesla-linked?status=...), which
// ASWebAuthenticationSession intercepts to return control to the iOS app. No
// tokens are ever placed in the redirect URL.
func (h *Handler) ServeCallback(w http.ResponseWriter, r *http.Request) {
	res := h.resolveCallback(r.Context(), r.URL.Query())
	if res.status == statusSuccess {
		h.logger.Info("tesla link callback: success")
	} else {
		h.logger.Warn("tesla link callback: failed",
			slog.String("reason", res.reason), slog.String("detail", res.detail))
	}
	h.redirect(w, res)
}

// callbackResult is the classified outcome of a callback request.
type callbackResult struct {
	status string // statusSuccess | statusError
	reason string // machine-readable error code; empty on success
	// detail is an ADDITIVE sub-classification of reason, emitted as its own
	// query parameter so the existing `reason` vocabulary (§7.11.2) is
	// unchanged and a client that has never heard of it is unaffected. It
	// exists because `invalid_state` conflated four different events, one of
	// which — a state that simply timed out — is a clean silent re-entry into
	// /start rather than something to make an owner read and act on.
	detail string
}

const (
	statusSuccess = "success"
	statusError   = "error"

	reasonTeslaDenied    = "tesla_denied"
	reasonInvalidState   = "invalid_state"
	reasonMissingCode    = "missing_code"
	reasonExchangeFailed = "exchange_failed"
	reasonNotProvisioned = "account_not_provisioned"
	reasonPersistFailed  = "persist_failed"

	// detailSessionExpired — the link session existed and its TTL elapsed
	// before Tesla redirected back. The owner did nothing wrong; the correct
	// client behaviour is to start a new link silently.
	detailSessionExpired = "session_expired"
	// detailSessionUnknown — no session for this state at all: a replay, a
	// forgery, or a process restart between /start and the callback.
	detailSessionUnknown = "session_unknown"
)

// resolveCallback runs the full callback classification: it surfaces a
// Tesla-reported error, validates + consumes the single-use state session,
// requires an authorization code, exchanges it, and persists the tokens for the
// session's user. It is a method (not a pure function) because it drives the
// exchange + persist seams, but it takes no dependency on http beyond the query
// values, so it is directly table-testable.
func (h *Handler) resolveCallback(ctx context.Context, q url.Values) callbackResult {
	if errCode := q.Get("error"); errCode != "" {
		// Burn any live session for this state so a denied attempt cannot be
		// replayed for the remainder of its TTL.
		h.sessions.Take(q.Get("state"))
		return callbackResult{status: statusError, reason: reasonTeslaDenied}
	}

	state := q.Get("state")
	sess, taken := h.sessions.Take(state)
	switch taken {
	case TakeExpired:
		// The owner completed a real consent flow and we threw the answer away
		// because our own clock ran out. Named separately so it is greppable in
		// prod and actionable in the client, and logged at Info because it is a
		// user-journey fact rather than a server fault.
		h.logger.Info("tesla link callback: link session expired before the consent flow finished",
			slog.String("event", "tesla_link_session_expired"),
			slog.Duration("ttl", h.sessions.TTL()))
		return callbackResult{status: statusError, reason: reasonInvalidState, detail: detailSessionExpired}
	case TakeUnknown:
		return callbackResult{status: statusError, reason: reasonInvalidState, detail: detailSessionUnknown}
	case TakeOK:
	}

	code := q.Get("code")
	if code == "" {
		return callbackResult{status: statusError, reason: reasonMissingCode}
	}

	tok, err := h.exchange(ctx, h.logger, h.cfg.ClientID, h.cfg.ClientSecret, h.cfg.RedirectURI, code, sess.PKCEVerifier)
	if err != nil {
		h.logger.Warn("tesla link callback: token exchange failed", slog.String("error", err.Error()))
		return callbackResult{status: statusError, reason: reasonExchangeFailed}
	}

	expiresAt := h.now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix()
	if err := h.linker.UpdateTeslaToken(ctx, sess.UserID, tok.AccessToken, tok.RefreshToken, expiresAt); err != nil {
		if errors.Is(err, ErrAccountNotProvisioned) {
			return callbackResult{status: statusError, reason: reasonNotProvisioned}
		}
		h.logger.Error("tesla link callback: persist failed",
			slog.String("user_id", sess.UserID), slog.String("error", err.Error()))
		return callbackResult{status: statusError, reason: reasonPersistFailed}
	}

	h.logger.Info("tesla account linked in-app", slog.String("user_id", sess.UserID))
	return callbackResult{status: statusSuccess}
}

// appRedirectURL builds the app deep link the callback hands off to. Query
// params carry only the outcome — never tokens or PII.
func appRedirectURL(base, status, reason, detail string) string {
	q := url.Values{}
	q.Set("status", status)
	if reason != "" {
		q.Set("reason", reason)
	}
	if detail != "" {
		q.Set("detail", detail)
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + q.Encode()
}

// redirect issues a 302 to the app deep link plus a tiny HTML fallback page
// (with a tappable link + meta refresh) in case the client does not follow the
// custom-scheme Location header automatically.
func (h *Handler) redirect(w http.ResponseWriter, res callbackResult) {
	target := appRedirectURL(h.cfg.AppRedirectURL, res.status, res.reason, res.detail)
	w.Header().Set("Location", target)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusFound)
	safe := html.EscapeString(target)
	title := "Tesla linked"
	body := "You can return to the MyRoboTaxi app."
	if res.status != statusSuccess {
		title = "Tesla link failed"
		body = "Return to the MyRoboTaxi app and try again."
	}
	fmt.Fprintf(w,
		`<!doctype html><html><head><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=%[1]s"><title>%[2]s</title><style>body{font:16px/1.4 system-ui,sans-serif;max-width:480px;margin:48px auto;padding:0 16px}</style></head><body><h1>%[2]s</h1><p>%[3]s</p><p><a href="%[1]s">Open MyRoboTaxi</a></p></body></html>`,
		safe, html.EscapeString(title), html.EscapeString(body),
	)
}

// bearerToken extracts the token from an "Authorization: Bearer <token>"
// header, returning "" when absent or malformed.
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	authz := r.Header.Get("Authorization")
	if !strings.HasPrefix(authz, prefix) {
		return ""
	}
	return authz[len(prefix):]
}

func (h *Handler) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		h.logger.Error("tesla link: writeJSON encode failed", slog.String("error", err.Error()))
	}
}

func (h *Handler) writeError(w http.ResponseWriter, status int, code wserrors.ErrorCode, msg string) {
	wserrors.WriteErrorEnvelope(w, h.logger, status, code, msg)
}
