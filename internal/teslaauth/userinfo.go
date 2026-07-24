package teslaauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// userInfoURL is Tesla's OIDC userinfo endpoint. It returns the stable Tesla
// subject (`sub`) for the bearer access token — the value NextAuth persists as
// Account.providerAccountId, so provisioning an Account row server-side with the
// same value stays collision-safe against a later web link on the same Tesla
// account.
const userInfoURL = "https://auth.tesla.com/oauth2/v3/userinfo"

// UserInfoEndpoint holds the userinfo URL in a package-level var (not const) so
// tests can redirect it to an httptest.Server. Production reads it unchanged.
var UserInfoEndpoint = userInfoURL

// userInfoTimeout bounds the userinfo roundtrip. The parent context also
// cancels; this floor protects against a hung connection to auth.tesla.com.
const userInfoTimeout = 15 * time.Second

// UserInfo is the subset of the Tesla OIDC userinfo response the provisioning
// path needs. Sub is the provider-scoped Tesla subject (opaque, P0); Email is
// the account email (P1, never logged) used only as a fallback display value.
type UserInfo struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
}

// FetchUserInfo calls Tesla's OIDC userinfo endpoint with the freshly minted
// access token and returns the Tesla subject. The logger records only status
// codes on failure — never the token or response body's PII.
func FetchUserInfo(ctx context.Context, logger *slog.Logger, accessToken string) (UserInfo, error) {
	if strings.TrimSpace(accessToken) == "" {
		return UserInfo{}, errors.New("teslaauth.FetchUserInfo: empty access token")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, UserInfoEndpoint, http.NoBody)
	if err != nil {
		return UserInfo{}, fmt.Errorf("teslaauth.FetchUserInfo: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: userInfoTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return UserInfo{}, fmt.Errorf("teslaauth.FetchUserInfo: get userinfo: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return UserInfo{}, fmt.Errorf("teslaauth.FetchUserInfo: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		logger.Warn("tesla userinfo request failed", slog.Int("status", resp.StatusCode))
		return UserInfo{}, fmt.Errorf("teslaauth.FetchUserInfo: tesla returned %d", resp.StatusCode)
	}

	var info UserInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return UserInfo{}, fmt.Errorf("teslaauth.FetchUserInfo: decode response: %w", err)
	}
	if strings.TrimSpace(info.Sub) == "" {
		return UserInfo{}, errors.New("teslaauth.FetchUserInfo: userinfo response missing sub")
	}
	return info, nil
}
