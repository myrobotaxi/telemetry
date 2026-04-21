package main

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

// tokenExchangeTimeout bounds the code → token HTTP roundtrip. The parent
// context also cancels the request, but this floor protects against a
// hung TCP connection to auth.tesla.com.
const tokenExchangeTimeout = 15 * time.Second

// teslaOAuthTokenEndpoint holds the Tesla /oauth2/v3/token URL in a
// package-level var (rather than const) so tests can redirect it to an
// httptest.Server. Production code reads it unchanged.
var teslaOAuthTokenEndpoint = teslaOAuthTokenURL

// pkcePair holds a PKCE verifier/challenge pair for a single OAuth flow.
type pkcePair struct {
	verifier  string
	challenge string
}

// newPKCE generates a fresh PKCE verifier + S256 challenge per RFC 7636.
// The verifier is a URL-safe random string; the challenge is
// base64url(sha256(verifier)) without padding.
func newPKCE() (pkcePair, error) {
	verifier, err := randomURLSafeString(32)
	if err != nil {
		return pkcePair{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return pkcePair{verifier: verifier, challenge: challenge}, nil
}

// randomURLSafeString returns n bytes of cryptographic randomness encoded
// with unpadded base64url, producing a string safe to use in URLs and
// query parameters.
func randomURLSafeString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand.Read: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// buildAuthorizeURL constructs the Tesla /oauth2/v3/authorize URL that
// starts the authorization_code + PKCE flow.
func buildAuthorizeURL(clientID, redirectURI, scopes, state, codeChallenge string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", scopes)
	q.Set("state", state)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	return teslaOAuthAuthorizeURL + "?" + q.Encode()
}

// tokenResponse mirrors Tesla's /oauth2/v3/token response body.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// exchangeCodeForToken swaps the one-time authorization code (plus PKCE
// verifier) for an access_token / refresh_token pair from Tesla.
func exchangeCodeForToken(
	ctx context.Context,
	logger *slog.Logger,
	clientID, clientSecret, redirectURI, code, codeVerifier string,
) (*tokenResponse, error) {
	form := buildTokenExchangeForm(clientID, clientSecret, redirectURI, code, codeVerifier)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, teslaOAuthTokenEndpoint, strings.NewReader(form.Encode()))
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

	var tok tokenResponse
	if err := json.Unmarshal(body, &tok); err != nil {
		return nil, fmt.Errorf("decode token response: %w", err)
	}
	if tok.AccessToken == "" || tok.RefreshToken == "" {
		return nil, errors.New("tesla response missing access_token or refresh_token")
	}
	return &tok, nil
}

// buildTokenExchangeForm assembles the x-www-form-urlencoded body for the
// Tesla code → token POST. Exposed separately so the exact parameter set
// is testable without spinning up an HTTP server.
func buildTokenExchangeForm(clientID, clientSecret, redirectURI, code, codeVerifier string) url.Values {
	form := url.Values{}
	form.Set("grant_type", "authorization_code")
	form.Set("client_id", clientID)
	form.Set("client_secret", clientSecret)
	form.Set("code", code)
	form.Set("redirect_uri", redirectURI)
	form.Set("code_verifier", codeVerifier)
	return form
}
