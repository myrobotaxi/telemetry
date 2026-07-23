package teslaauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestGeneratePKCE(t *testing.T) {
	pkce, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("GeneratePKCE: %v", err)
	}
	if pkce.Verifier == "" || pkce.Challenge == "" {
		t.Fatalf("empty pkce pair: %+v", pkce)
	}

	// challenge must be base64url(sha256(verifier)) with no padding.
	sum := sha256.Sum256([]byte(pkce.Verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if pkce.Challenge != want {
		t.Errorf("challenge mismatch: got %q, want %q", pkce.Challenge, want)
	}

	// Two consecutive pkce pairs must differ.
	other, err := GeneratePKCE()
	if err != nil {
		t.Fatalf("second GeneratePKCE: %v", err)
	}
	if other.Verifier == pkce.Verifier {
		t.Error("expected distinct verifiers across pkce pairs")
	}
}

func TestRandomURLSafeString(t *testing.T) {
	tests := []struct {
		name string
		n    int
	}{
		{"state-24", 24},
		{"pkce-32", 32},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := RandomURLSafeString(tt.n)
			if err != nil {
				t.Fatalf("RandomURLSafeString: %v", err)
			}
			b, err := RandomURLSafeString(tt.n)
			if err != nil {
				t.Fatalf("RandomURLSafeString: %v", err)
			}
			if a == "" || a == b {
				t.Errorf("expected non-empty distinct strings, got %q and %q", a, b)
			}
			// base64url without padding must not contain +, /, or =.
			if strings.ContainsAny(a, "+/=") {
				t.Errorf("not url-safe: %q", a)
			}
		})
	}
}

func TestBuildAuthorizeURL(t *testing.T) {
	urlStr := BuildAuthorizeURL(
		"client-123",
		"https://telemetry.myrobotaxi.app/api/tesla/link/callback",
		FullScopes,
		"state-xyz",
		"challenge-abc",
	)

	u, err := url.Parse(urlStr)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != AuthorizeURL {
		t.Errorf("endpoint: got %q, want %q", got, AuthorizeURL)
	}
	q := u.Query()
	checks := map[string]string{
		"response_type":         "code",
		"client_id":             "client-123",
		"redirect_uri":          "https://telemetry.myrobotaxi.app/api/tesla/link/callback",
		"scope":                 FullScopes,
		"state":                 "state-xyz",
		"code_challenge":        "challenge-abc",
		"code_challenge_method": "S256",
		// MYR-242: always request re-consent for missing scopes.
		"prompt_missing_scopes": "true",
	}
	for k, want := range checks {
		if got := q.Get(k); got != want {
			t.Errorf("param %s: got %q, want %q", k, got, want)
		}
	}
}

func TestBuildTokenExchangeForm(t *testing.T) {
	form := BuildTokenExchangeForm("cid", "csec", "https://cb/callback", "the-code", "pkce-verifier")

	want := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     "cid",
		"client_secret": "csec",
		"code":          "the-code",
		"redirect_uri":  "https://cb/callback",
		"code_verifier": "pkce-verifier",
	}
	for k, v := range want {
		if got := form.Get(k); got != v {
			t.Errorf("form[%s]: got %q, want %q", k, got, v)
		}
	}
}

func TestExchangeCodeForToken(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantErr    string
		wantAccess string
	}{
		{
			name:   "success",
			status: http.StatusOK,
			// Literal JSON avoids the gosec G117 literal-struct credential scanner.
			body:       `{"access_token":"fake-access","refresh_token":"fake-refresh","expires_in":3600,"token_type":"Bearer"}`,
			wantAccess: "fake-access",
		},
		{
			name:    "non-200 surfaces status and body",
			status:  http.StatusUnauthorized,
			body:    `{"error":"invalid_grant"}`,
			wantErr: "401",
		},
		{
			name:    "missing token fields rejected",
			status:  http.StatusOK,
			body:    `{"access_token":"","refresh_token":"","expires_in":0}`,
			wantErr: "missing access_token",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotForm url.Values
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				gotForm, _ = url.ParseQuery(string(body))
				if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
					t.Errorf("content-type: got %q", ct)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			withTokenEndpoint(t, srv.URL)

			tok, err := ExchangeCodeForToken(
				context.Background(), slog.Default(),
				"cid", "csec", "https://cb/callback", "the-code", "pkce-verifier",
			)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("want error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ExchangeCodeForToken: %v", err)
			}
			if tok.AccessToken != tt.wantAccess {
				t.Errorf("access token: got %q, want %q", tok.AccessToken, tt.wantAccess)
			}
			// The request must carry the fully-assembled form.
			if got := gotForm.Get("grant_type"); got != "authorization_code" {
				t.Errorf("form grant_type: got %q", got)
			}
			if got := gotForm.Get("code_verifier"); got != "pkce-verifier" {
				t.Errorf("form code_verifier: got %q", got)
			}
		})
	}
}

// withTokenEndpoint points the Tesla token endpoint at a test server for the
// duration of a single test and restores the production URL at the end.
func withTokenEndpoint(t *testing.T, endpoint string) {
	t.Helper()
	prev := TokenEndpoint
	TokenEndpoint = endpoint
	t.Cleanup(func() { TokenEndpoint = prev })
}
