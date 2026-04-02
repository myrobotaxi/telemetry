package telemetry

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestTokenRefresher_Refresh(t *testing.T) {
	tests := []struct {
		name         string
		refreshToken string
		serverStatus int
		serverBody   string
		wantErr      bool
		wantAccess   string
		wantRefresh  string
	}{
		{
			name:         "successful refresh",
			refreshToken: "valid-refresh-token",
			serverStatus: http.StatusOK,
			serverBody: `{
				"access_token": "new-access-token",
				"refresh_token": "new-refresh-token",
				"expires_in": 28800,
				"token_type": "Bearer"
			}`,
			wantErr:     false,
			wantAccess:  "new-access-token",
			wantRefresh: "new-refresh-token",
		},
		{
			name:         "empty refresh token",
			refreshToken: "",
			serverStatus: http.StatusOK,
			serverBody:   `{}`,
			wantErr:      true,
		},
		{
			name:         "Tesla returns 400 (invalid grant)",
			refreshToken: "expired-refresh-token",
			serverStatus: http.StatusBadRequest,
			serverBody:   `{"error":"invalid_grant","error_description":"refresh token expired"}`,
			wantErr:      true,
		},
		{
			name:         "Tesla returns 401 (unauthorized)",
			refreshToken: "bad-refresh-token",
			serverStatus: http.StatusUnauthorized,
			serverBody:   `{"error":"unauthorized"}`,
			wantErr:      true,
		},
		{
			name:         "Tesla returns 500 (server error)",
			refreshToken: "valid-refresh-token",
			serverStatus: http.StatusInternalServerError,
			serverBody:   `{"error":"internal error"}`,
			wantErr:      true,
		},
		{
			name:         "empty access_token in response",
			refreshToken: "valid-refresh-token",
			serverStatus: http.StatusOK,
			serverBody:   `{"access_token":"","refresh_token":"rt","expires_in":3600}`,
			wantErr:      true,
		},
		{
			name:         "invalid JSON response",
			refreshToken: "valid-refresh-token",
			serverStatus: http.StatusOK,
			serverBody:   `not-json`,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedForm map[string]string

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Errorf("expected POST, got %s", r.Method)
				}

				ct := r.Header.Get("Content-Type")
				if ct != "application/x-www-form-urlencoded" {
					t.Errorf("Content-Type: got %q, want application/x-www-form-urlencoded", ct)
				}

				r.Body = http.MaxBytesReader(w, r.Body, 1<<20) //nolint:gosec // test only
				if err := r.ParseForm(); err != nil {
					t.Fatalf("ParseForm: %v", err)
				}
				capturedForm = map[string]string{
					"grant_type":    r.FormValue("grant_type"),
					"client_id":     r.FormValue("client_id"),
					"client_secret": r.FormValue("client_secret"),
					"refresh_token": r.FormValue("refresh_token"),
				}

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.serverStatus)
				fmt.Fprint(w, tt.serverBody)
			}))
			t.Cleanup(srv.Close)

			// Create refresher pointing at our test server.
			refresher := NewTokenRefresher(TeslaOAuthConfig{
				ClientID:     "test-client-id",
				ClientSecret: "test-client-secret",
			}, discardLogger())
			refresher.endpoint = srv.URL // inject test server URL

			result, err := refresher.Refresh(context.Background(), tt.refreshToken)

			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if result.AccessToken != tt.wantAccess {
				t.Errorf("access_token: got %q, want %q", result.AccessToken, tt.wantAccess)
			}
			if result.RefreshToken != tt.wantRefresh {
				t.Errorf("refresh_token: got %q, want %q", result.RefreshToken, tt.wantRefresh)
			}

			// Verify the form values sent to Tesla.
			if capturedForm["grant_type"] != "refresh_token" {
				t.Errorf("grant_type: got %q, want %q", capturedForm["grant_type"], "refresh_token")
			}
			if capturedForm["client_id"] != "test-client-id" {
				t.Errorf("client_id: got %q, want %q", capturedForm["client_id"], "test-client-id")
			}
			if capturedForm["client_secret"] != "test-client-secret" {
				t.Errorf("client_secret: got %q, want %q", capturedForm["client_secret"], "test-client-secret")
			}
			if capturedForm["refresh_token"] != tt.refreshToken {
				t.Errorf("refresh_token: got %q, want %q", capturedForm["refresh_token"], tt.refreshToken)
			}
		})
	}
}

func TestTeslaRefreshedToken_ExpiresAt(t *testing.T) {
	before := time.Now()
	tok := TeslaRefreshedToken{ExpiresIn: 3600}
	got := tok.ExpiresAt()
	after := time.Now()

	// ExpiresAt should be ~1 hour from now.
	wantMin := before.Add(3600 * time.Second)
	wantMax := after.Add(3600 * time.Second)

	if got.Before(wantMin) || got.After(wantMax) {
		t.Errorf("ExpiresAt() = %v, want between %v and %v", got, wantMin, wantMax)
	}
}
