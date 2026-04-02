package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// TeslaOAuthConfig holds the credentials needed to refresh Tesla OAuth tokens.
type TeslaOAuthConfig struct {
	ClientID     string
	ClientSecret string
}

// TeslaRefreshedToken contains the new token set returned by Tesla after a
// successful token refresh.
type TeslaRefreshedToken struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64 // seconds until expiry
}

// ExpiresAt returns the absolute expiry time based on the current time and
// the token's ExpiresIn duration.
func (t TeslaRefreshedToken) ExpiresAt() time.Time {
	return time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
}

// TokenRefresher refreshes expired Tesla OAuth tokens using the Tesla
// authorization server's refresh_token grant.
type TokenRefresher struct {
	config     TeslaOAuthConfig
	httpClient *http.Client
	logger     *slog.Logger
	// oauthEndpoint is the Tesla OAuth2 endpoint URL. Always set to the
	// production endpoint in NewTokenRefresher; tests override for httptest.
	oauthEndpoint string
}

// NewTokenRefresher creates a TokenRefresher that calls Tesla's OAuth2 endpoint
// to exchange a refresh_token for a new access_token. The config must contain
// valid ClientID and ClientSecret values.
func NewTokenRefresher(cfg TeslaOAuthConfig, logger *slog.Logger) *TokenRefresher {
	return &TokenRefresher{
		config:     cfg,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		logger:     logger,
		oauthEndpoint:   "https://auth.tesla.com/oauth2/v3/token",	}
}

// Refresh exchanges the given refresh token for a new token set by calling
// Tesla's OAuth2 token endpoint. Returns the refreshed tokens or an error
// describing the failure.
func (r *TokenRefresher) Refresh(ctx context.Context, refreshToken string) (TeslaRefreshedToken, error) {
	if refreshToken == "" {
		return TeslaRefreshedToken{}, fmt.Errorf("TokenRefresher.Refresh: refresh token is empty")
	}

	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {r.config.ClientID},
		"client_secret": {r.config.ClientSecret},
		"refresh_token": {refreshToken},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		r.oauthEndpoint,		strings.NewReader(form.Encode()))
	if err != nil {
		return TeslaRefreshedToken{}, fmt.Errorf("TokenRefresher.Refresh: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := r.httpClient.Do(req)	if err != nil {
		return TeslaRefreshedToken{}, fmt.Errorf("TokenRefresher.Refresh: http request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return TeslaRefreshedToken{}, fmt.Errorf("TokenRefresher.Refresh: read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		r.logger.Warn("Tesla token refresh failed",
			slog.Int("status", resp.StatusCode),
			slog.String("body", string(body)),
		)
		return TeslaRefreshedToken{}, fmt.Errorf("TokenRefresher.Refresh: Tesla returned %d: %s",
			resp.StatusCode, string(body))
	}

	var tokenResp teslaTokenResponse
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return TeslaRefreshedToken{}, fmt.Errorf("TokenRefresher.Refresh: decode response: %w", err)
	}

	if tokenResp.AccessToken == "" {
		return TeslaRefreshedToken{}, fmt.Errorf("TokenRefresher.Refresh: empty access_token in response")
	}

	r.logger.Info("Tesla token refreshed successfully",
		slog.Int64("expires_in", tokenResp.ExpiresIn),
	)

	return TeslaRefreshedToken{
		AccessToken:  tokenResp.AccessToken,
		RefreshToken: tokenResp.RefreshToken,
		ExpiresIn:    tokenResp.ExpiresIn,
	}, nil
}

// teslaTokenResponse is the JSON response from Tesla's OAuth2 token endpoint.
type teslaTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}
