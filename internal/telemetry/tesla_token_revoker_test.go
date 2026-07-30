package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestTokenRevoker_RevokeWithEndpoint pins the transport contract of the
// MYR-366 revocation call. Every case goes through an httptest.Server — the
// same convention TestTokenRefresher_Refresh uses — so no unit test can reach
// auth.tesla.com.
func TestTokenRevoker_RevokeWithEndpoint(t *testing.T) {
	tests := []struct {
		name         string
		refreshToken string
		serverStatus int
		serverBody   string
		wantErr      bool
	}{
		{
			name:         "Tesla accepts the revocation",
			refreshToken: "rt-live",
			serverStatus: http.StatusOK,
			serverBody:   `{}`,
		},
		{
			name:         "204 is also success",
			refreshToken: "rt-live",
			serverStatus: http.StatusNoContent,
		},
		{
			name: "already-invalid token is success (RFC 7009 §2.2)",
			// Tesla answers 200 for a token it no longer knows. The caller
			// must not treat the re-run case as a failure.
			refreshToken: "rt-already-dead",
			serverStatus: http.StatusOK,
			serverBody:   `{}`,
		},
		{
			name:         "empty refresh token never leaves the process",
			refreshToken: "",
			serverStatus: http.StatusOK,
			wantErr:      true,
		},
		{
			name:         "Tesla 400 is an error",
			refreshToken: "rt-live",
			serverStatus: http.StatusBadRequest,
			serverBody:   `{"error":"invalid_request"}`,
			wantErr:      true,
		},
		{
			name:         "Tesla 401 is an error",
			refreshToken: "rt-live",
			serverStatus: http.StatusUnauthorized,
			serverBody:   `{"error":"unauthorized"}`,
			wantErr:      true,
		},
		{
			name:         "Tesla 500 is an error",
			refreshToken: "rt-live",
			serverStatus: http.StatusInternalServerError,
			serverBody:   `{"error":"internal"}`,
			wantErr:      true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.serverStatus)
				_, _ = w.Write([]byte(tc.serverBody))
			}))
			defer srv.Close()

			r := NewTokenRevoker(TeslaOAuthConfig{ClientID: "cid", ClientSecret: "secret"}, discardLogger())
			err := r.revokeWithEndpoint(context.Background(), srv.URL, tc.refreshToken)

			if tc.wantErr && err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestTokenRevoker_RequestShape pins what actually goes on the wire: a POST,
// form-encoded, carrying the REFRESH token (which revokes the whole grant) and
// the client id.
func TestTokenRevoker_RequestShape(t *testing.T) {
	var (
		gotMethod string
		gotType   string
		gotForm   url.Values
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotMethod = req.Method
		gotType = req.Header.Get("Content-Type")
		body, _ := io.ReadAll(req.Body)
		gotForm, _ = url.ParseQuery(string(body))
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	r := NewTokenRevoker(TeslaOAuthConfig{ClientID: "cid-42", ClientSecret: "secret"}, discardLogger())
	if err := r.revokeWithEndpoint(context.Background(), srv.URL, "rt-live"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if !strings.HasPrefix(gotType, "application/x-www-form-urlencoded") {
		t.Errorf("Content-Type = %q", gotType)
	}
	for field, want := range map[string]string{
		"token":           "rt-live",
		"token_type_hint": "refresh_token",
		"client_id":       "cid-42",
	} {
		if got := gotForm.Get(field); got != want {
			t.Errorf("form[%s] = %q, want %q", field, got, want)
		}
	}
	// The secret is NOT sent: RFC 7009 identifies the client by id, and this
	// endpoint is reached from a deletion path that must work regardless.
	if got := gotForm.Get("client_secret"); got != "" {
		t.Errorf("client_secret was sent (%q); revoke takes client_id only", got)
	}
}

// TestTokenRevoker_ProductionEndpoint pins the endpoint constant, so a typo or
// a host change cannot land silently. Revocation lives on the SAME auth host
// as authorize/token.
func TestTokenRevoker_ProductionEndpoint(t *testing.T) {
	const want = "https://auth.tesla.com/oauth2/v3/revoke"
	if teslaOAuthRevokeEndpoint != want {
		t.Fatalf("teslaOAuthRevokeEndpoint = %q, want %q", teslaOAuthRevokeEndpoint, want)
	}
}
