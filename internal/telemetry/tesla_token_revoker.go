package telemetry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// teslaOAuthRevokeEndpoint is Tesla's OAuth2 token-revocation endpoint (RFC
// 7009), on the SAME auth host as the authorize/token endpoints used by
// teslaauth.AuthorizeURL and TokenRefresher.
//
// #nosec G101 -- public OAuth endpoint URL, not a credential.
const teslaOAuthRevokeEndpoint = "https://auth.tesla.com/oauth2/v3/revoke"

// revokeTimeout bounds the revoke roundtrip. Revocation is best-effort and
// sits in front of a destructive local delete the user is waiting on, so the
// ceiling is deliberately tighter than the 15s refresh timeout: a slow Tesla
// must not add fifteen seconds to "Delete my account".
const revokeTimeout = 5 * time.Second

// maxRevokeErrorBody caps how much of a failed revoke's response body is quoted
// into the returned error, and therefore into the caller's log line.
const maxRevokeErrorBody = 1 << 10 // 1 KiB

// TokenRevoker actively revokes a stored Tesla OAuth grant at Tesla's
// authorization server (MYR-366). It is the machine-callable counterpart to
// the owner-confirmed consent page (teslaConsentRevokeBase, car-offboarding.md
// §1.2): that page needs a human in a browser, this needs only the refresh
// token we already hold, so it can run unattended in the deletion paths.
//
// Presenting the REFRESH token revokes the whole grant — per RFC 7009 §2.1 an
// authorization server that receives a refresh token SHOULD invalidate the
// access tokens issued from it as well, which is exactly the outcome the
// severing paths want.
//
// It never logs token material: only the HTTP status and Tesla's own error
// body reach the log, and the token travels in the POST body, which is never
// logged.
type TokenRevoker struct {
	config     TeslaOAuthConfig
	httpClient *http.Client
	logger     *slog.Logger
}

// NewTokenRevoker creates a TokenRevoker for the given OAuth client. Only
// ClientID is sent (RFC 7009 takes the client id; Tesla does not require the
// secret on this endpoint) — the config type is shared with TokenRefresher so
// the composition root passes one credential struct to both.
func NewTokenRevoker(cfg TeslaOAuthConfig, logger *slog.Logger) *TokenRevoker {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &TokenRevoker{
		config:     cfg,
		httpClient: &http.Client{Timeout: revokeTimeout},
		logger:     logger,
	}
}

// Revoke revokes the grant behind refreshToken. A nil error means Tesla
// accepted the revocation; every caller treats an error as non-fatal.
func (r *TokenRevoker) Revoke(ctx context.Context, refreshToken string) error {
	return r.revokeWithEndpoint(ctx, teslaOAuthRevokeEndpoint, refreshToken)
}

// revokeWithEndpoint is the internal implementation that accepts an explicit
// endpoint URL. Revoke() always passes the hardcoded const; tests call this
// directly with an httptest.Server URL (same convention as
// TokenRefresher.refreshWithEndpoint), so no unit test can reach Tesla.
func (r *TokenRevoker) revokeWithEndpoint(ctx context.Context, endpoint, refreshToken string) error {
	if refreshToken == "" {
		return fmt.Errorf("TokenRevoker.Revoke: refresh token is empty")
	}

	form := url.Values{
		"token":           {refreshToken},
		"token_type_hint": {"refresh_token"},
		"client_id":       {r.config.ClientID},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("TokenRevoker.Revoke: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("TokenRevoker.Revoke: http request: %w", err)
	}
	defer resp.Body.Close()

	// Deliberately a much tighter cap than the 64 KiB maxResponseBody the rest
	// of this package reads with. This body is quoted verbatim into an error
	// that a caller logs; RFC 6749 §5.2 says it carries only
	// error/error_description/error_uri, but a 1 KiB ceiling means a
	// misbehaving or compromised endpoint cannot echo an unbounded blob —
	// including our own token — into the log through us.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxRevokeErrorBody))
	if err != nil {
		return fmt.Errorf("TokenRevoker.Revoke: read response: %w", err)
	}

	// RFC 7009 §2.2: an already-invalid token is a SUCCESS, and the server
	// answers 200 for it. Any 2xx is therefore "the grant is gone", which is
	// all the caller wanted to know.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("TokenRevoker.Revoke: Tesla returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}
