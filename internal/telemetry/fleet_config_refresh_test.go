package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- Refresh test doubles ---

type stubTeslaTokenRefresher struct {
	result TeslaRefreshedToken
	err    error
	called bool
}

func (s *stubTeslaTokenRefresher) Refresh(_ context.Context, _ string) (TeslaRefreshedToken, error) {
	s.called = true
	return s.result, s.err
}

type stubTeslaTokenUpdater struct {
	called       bool
	accessToken  string
	refreshToken string
	expiresAt    int64
	userID       string
	err          error
}

func (s *stubTeslaTokenUpdater) UpdateTeslaToken(_ context.Context, userID, accessToken, refreshToken string, expiresAt int64) error {
	s.called = true
	s.userID = userID
	s.accessToken = accessToken
	s.refreshToken = refreshToken
	s.expiresAt = expiresAt
	return s.err
}

// --- Tests ---

func TestFleetConfigHandler_TokenAutoRefresh(t *testing.T) {
	const (
		validVIN    = "5YJ3E1EA1PF000001"
		userID      = "user-123"
		authToken   = "valid-token"
		successBody = `{"response":{"updated_vehicles":1,"skipped_vehicles":{}}}`
	)

	tests := []struct {
		name           string
		token          TeslaToken
		refresher      *stubTeslaTokenRefresher
		updater        *stubTeslaTokenUpdater
		wantStatus     int
		wantError      string
		wantRefreshed  bool
		wantUpdated    bool
		wantFleetToken string // expected token sent to Fleet API
	}{
		{
			name: "expired token with successful refresh",
			token: TeslaToken{ //nolint:gosec // test fixture
				AccessToken:  "expired-token",
				RefreshToken: "valid-refresh-token",
				ExpiresAt:    time.Now().Add(-time.Hour),
			},
			refresher: &stubTeslaTokenRefresher{
				result: TeslaRefreshedToken{
					AccessToken:  "new-access-token",
					RefreshToken: "new-refresh-token",
					ExpiresIn:    28800,
				},
			},
			updater:        &stubTeslaTokenUpdater{},
			wantStatus:     http.StatusOK,
			wantRefreshed:  true,
			wantUpdated:    true,
			wantFleetToken: "new-access-token",
		},
		{
			name: "expired token, refresh fails (400 from Tesla)",
			token: TeslaToken{ //nolint:gosec // test fixture
				AccessToken:  "expired-token",
				RefreshToken: "bad-refresh-token",
				ExpiresAt:    time.Now().Add(-time.Hour),
			},
			refresher: &stubTeslaTokenRefresher{
				err: errors.New("Tesla returned 400: invalid_grant"),
			},
			updater:       &stubTeslaTokenUpdater{},
			wantStatus:    http.StatusUnauthorized,
			wantError:     "Tesla token expired",
			wantRefreshed: true,
			wantUpdated:   false,
		},
		{
			name: "expired token, no refresh token available",
			token: TeslaToken{ //nolint:gosec // test fixture
				AccessToken: "expired-token",
				ExpiresAt:   time.Now().Add(-time.Hour),
			},
			refresher:     &stubTeslaTokenRefresher{},
			updater:       &stubTeslaTokenUpdater{},
			wantStatus:    http.StatusUnauthorized,
			wantError:     "Tesla token expired",
			wantRefreshed: false,
			wantUpdated:   false,
		},
		{
			name: "expired token, no refresher configured",
			token: TeslaToken{ //nolint:gosec // test fixture
				AccessToken:  "expired-token",
				RefreshToken: "has-refresh-but-no-refresher",
				ExpiresAt:    time.Now().Add(-time.Hour),
			},
			refresher:     nil, // no refresher
			updater:       nil,
			wantStatus:    http.StatusUnauthorized,
			wantError:     "Tesla token expired",
			wantRefreshed: false,
			wantUpdated:   false,
		},
		{
			name: "expired token, refresh succeeds but DB update fails",
			token: TeslaToken{ //nolint:gosec // test fixture
				AccessToken:  "expired-token",
				RefreshToken: "valid-refresh-token",
				ExpiresAt:    time.Now().Add(-time.Hour),
			},
			refresher: &stubTeslaTokenRefresher{
				result: TeslaRefreshedToken{
					AccessToken:  "new-access-token",
					RefreshToken: "new-refresh-token",
					ExpiresIn:    28800,
				},
			},
			updater: &stubTeslaTokenUpdater{
				err: errors.New("connection refused"),
			},
			wantStatus:     http.StatusOK,
			wantRefreshed:  true,
			wantUpdated:    true,
			wantFleetToken: "new-access-token",
		},
		{
			name: "valid token, no refresh needed",
			token: TeslaToken{ //nolint:gosec // test fixture
				AccessToken:  "valid-access-token",
				RefreshToken: "refresh-token",
				ExpiresAt:    time.Now().Add(time.Hour),
			},
			refresher:      &stubTeslaTokenRefresher{},
			updater:        &stubTeslaTokenUpdater{},
			wantStatus:     http.StatusOK,
			wantRefreshed:  false,
			wantUpdated:    false,
			wantFleetToken: "valid-access-token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Track what token was sent to Fleet API.
			var capturedFleetAuth string
			fleetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capturedFleetAuth = r.Header.Get("Authorization")
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, successBody)
			}))
			t.Cleanup(fleetSrv.Close)

			var opts []FleetConfigOption
			if tt.refresher != nil {
				opts = append(opts, WithTokenRefresher(tt.refresher, tt.updater))
			}

			handler := NewFleetConfigHandler(
				&stubTokenValidator{userID: userID},
				&stubVehicleOwner{ownerID: userID},
				&stubTeslaTokenProvider{token: tt.token},
				newTestFleetClient(fleetSrv.URL),
				EndpointConfig{
					Hostname: "telemetry.example.com",
					Port:     443,
					CA:       "ca-cert",
				},
				discardLogger(),
				opts...,
			)

			mux := http.NewServeMux()
			mux.Handle("POST /api/fleet-config/{vin}", handler)

			req := httptest.NewRequestWithContext(
				context.Background(),
				http.MethodPost,
				"/api/fleet-config/"+validVIN,
				nil,
			)
			req.Header.Set("Authorization", "Bearer "+authToken)

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status code: got %d, want %d", rec.Code, tt.wantStatus)
			}

			if tt.wantError != "" {
				var errResp fleetConfigErrorResponse
				if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
					t.Fatalf("decode error response: %v", err)
				}
				if !strings.Contains(errResp.Error, tt.wantError) {
					t.Errorf("error: got %q, want substring %q", errResp.Error, tt.wantError)
				}
			}

			if tt.refresher != nil {
				if tt.refresher.called != tt.wantRefreshed {
					t.Errorf("refresher called: got %v, want %v", tt.refresher.called, tt.wantRefreshed)
				}
			}

			if tt.updater != nil {
				if tt.updater.called != tt.wantUpdated {
					t.Errorf("updater called: got %v, want %v", tt.updater.called, tt.wantUpdated)
				}
			}

			if tt.wantFleetToken != "" {
				wantAuth := "Bearer " + tt.wantFleetToken
				if capturedFleetAuth != wantAuth {
					t.Errorf("Fleet API auth: got %q, want %q", capturedFleetAuth, wantAuth)
				}
			}

			// Verify updater received correct values on successful refresh.
			if tt.wantUpdated && tt.updater != nil && tt.updater.called && tt.refresher != nil && tt.refresher.err == nil {
				if tt.updater.userID != userID {
					t.Errorf("updater userID: got %q, want %q", tt.updater.userID, userID)
				}
				if tt.updater.accessToken != tt.refresher.result.AccessToken {
					t.Errorf("updater accessToken: got %q, want %q",
						tt.updater.accessToken, tt.refresher.result.AccessToken)
				}
				if tt.updater.refreshToken != tt.refresher.result.RefreshToken {
					t.Errorf("updater refreshToken: got %q, want %q",
						tt.updater.refreshToken, tt.refresher.result.RefreshToken)
				}
			}
		})
	}
}
