// Package teslaauth holds the reusable Tesla Fleet OAuth2 authorization-code
// + PKCE primitives shared by the `ops auth link` CLI (cmd/ops) and the
// user-facing in-app link endpoints (internal/teslalink, MYR-246). The logic
// lived only in cmd/ops/auth_oauth.go before MYR-246; it is factored here so
// the server-side callback reuses the exact same authorize-URL construction
// and code->token exchange rather than duplicating it.
//
// This package performs NO persistence and holds NO secrets of its own — the
// caller supplies the client id/secret per call. It never logs token values.
package teslaauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// AuthorizeURL is Tesla's OAuth2 authorize endpoint.
	AuthorizeURL = "https://auth.tesla.com/oauth2/v3/authorize"

	// tokenURL is Tesla's OAuth2 token endpoint (shared with refresh_token grant).
	tokenURL = "https://auth.tesla.com/oauth2/v3/token" //#nosec G101 -- public OAuth endpoint URL, not a credential

	// FullScopes is the complete Fleet API scope set the user-facing link flow
	// requests (MYR-242/MYR-246): identity + refresh + profile + telemetry +
	// location + command + charging-command. prompt_missing_scopes=true (always
	// sent by BuildAuthorizeURL) forces Tesla to re-show consent for any scope
	// not already granted, so an already-linked owner re-consents to newly
	// requested scopes instead of silently keeping the old set.
	FullScopes = "openid offline_access user_data vehicle_device_data vehicle_location vehicle_cmds vehicle_charging_cmds"

	// tokenExchangeTimeout bounds the code->token HTTP roundtrip. The parent
	// context also cancels the request; this floor protects against a hung
	// TCP connection to auth.tesla.com.
	tokenExchangeTimeout = 15 * time.Second
)

// TokenEndpoint holds the Tesla /oauth2/v3/token URL in a package-level var
// (rather than const) so tests can redirect it to an httptest.Server.
// Production code reads it unchanged.
var TokenEndpoint = tokenURL

// PKCEPair holds a PKCE verifier/challenge pair for a single OAuth flow.
type PKCEPair struct {
	Verifier  string
	Challenge string
}

// GeneratePKCE generates a fresh PKCE verifier + S256 challenge per RFC 7636.
// The verifier is a URL-safe random string; the challenge is
// base64url(sha256(verifier)) without padding.
func GeneratePKCE() (PKCEPair, error) {
	verifier, err := RandomURLSafeString(32)
	if err != nil {
		return PKCEPair{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return PKCEPair{Verifier: verifier, Challenge: challenge}, nil
}

// RandomURLSafeString returns n bytes of cryptographic randomness encoded with
// unpadded base64url, producing a string safe to use in URLs and query
// parameters (PKCE verifiers, OAuth state).
func RandomURLSafeString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand.Read: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// BuildAuthorizeURL constructs the Tesla /oauth2/v3/authorize URL that starts
// the authorization_code + PKCE flow. It always sets prompt_missing_scopes=true
// (MYR-242): without it Tesla silently re-issues the previously consented scope
// set for an already-linked account and never shows consent for newly requested
// scopes.
func BuildAuthorizeURL(clientID, redirectURI, scopes, state, codeChallenge string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", scopes)
	q.Set("state", state)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	q.Set("prompt_missing_scopes", "true")
	return AuthorizeURL + "?" + q.Encode()
}

// TokenResponse mirrors Tesla's /oauth2/v3/token response body.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// ExchangeCodeForToken swaps the one-time authorization code (plus PKCE
// verifier) for an access_token / refresh_token pair from Tesla. The logger is
// used only to record a failed exchange's status/body — token values are never
// logged.
func ExchangeCodeForToken(
	ctx context.Context,
	logger *slog.Logger,
	clientID, clientSecret, redirectURI, code, codeVerifier string,
) (*TokenResponse, error) {
	form := BuildTokenExchangeForm(clientID, clientSecret, redirectURI, code, codeVerifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: tokenExchangeTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post to token endpoint: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil, fmt.Errorf("read token response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		logger.Warn("tesla token exchange failed",
			slog.Int("status", resp.StatusCode),
			slog.String("body", string(body)),
		)
		return nil, fmt.Errorf("tesla returned %d: %s", resp.StatusCode, string(body))
	}

	var tok TokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" {
		return nil, errors.New("tesla response missing access_token or refresh_token")
	}
	return &tok, nil
}

// BuildTokenExchangeForm assembles the x-www-form-urlencoded body for the Tesla
// code->token POST. Exposed separately so the exact parameter set is testable
// without spinning up an HTTP server.
func BuildTokenExchangeForm(clientID, clientSecret, redirectURI, code, codeVerifier string) url.Values {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", codeVerifier)
	return form
}
