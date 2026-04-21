package main

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
	"time"
)

func TestNewPKCE_ChallengeIsSha256OfVerifier(t *testing.T) {
	pkce, err := newPKCE()
	if err != nil {
		t.Fatalf("newPKCE: %v", err)
	}
	if pkce.verifier == "" || pkce.challenge == "" {
		t.Fatalf("empty pkce pair: %+v", pkce)
	}

	// challenge must be base64url(sha256(verifier)) with no padding.
	sum := sha256.Sum256([]byte(pkce.verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if pkce.challenge != want {
		t.Errorf("challenge mismatch: got %q, want %q", pkce.challenge, want)
	}

	// Two consecutive pkce pairs must differ.
	other, err := newPKCE()
	if err != nil {
		t.Fatalf("second newPKCE: %v", err)
	}
	if other.verifier == pkce.verifier {
		t.Error("expected distinct verifiers across pkce pairs")
	}
}

func TestBuildAuthorizeURL_ContainsAllRequiredParams(t *testing.T) {
	urlStr := buildAuthorizeURL(
		"client-123",
		"http://localhost:8765/callback",
		"openid offline_access",
		"state-xyz",
		"challenge-abc",
	)

	u, err := url.Parse(urlStr)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := u.Scheme + "://" + u.Host + u.Path; got != teslaOAuthAuthorizeURL {
		t.Errorf("endpoint: got %q, want %q", got, teslaOAuthAuthorizeURL)
	}
	q := u.Query()
	checks := map[string]string{
		"response_type":         "code",
		"client_id":             "client-123",
		"redirect_uri":          "http://localhost:8765/callback",
		"scope":                 "openid offline_access",
		"state":                 "state-xyz",
		"code_challenge":        "challenge-abc",
		"code_challenge_method": "S256",
	}
	for k, want := range checks {
		if got := q.Get(k); got != want {
			t.Errorf("param %s: got %q, want %q", k, got, want)
		}
	}
}

func TestBuildTokenExchangeForm_AssemblesAllFields(t *testing.T) {
	form := buildTokenExchangeForm("cid", "csec", "http://localhost:8765/callback", "the-code", "pkce-verifier")

	want := map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     "cid",
		"client_secret": "csec",
		"code":          "the-code",
		"redirect_uri":  "http://localhost:8765/callback",
		"code_verifier": "pkce-verifier",
	}
	for k, v := range want {
		if got := form.Get(k); got != v {
			t.Errorf("form[%s]: got %q, want %q", k, got, v)
		}
	}
}

func TestCallbackHandler_SuccessPath(t *testing.T) {
	result := make(chan callbackResult, 1)
	handler := callbackHandler("expected-state", result)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/callback?state=expected-state&code=code-xyz", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status: got %d, want 200", rec.Code)
	}
	select {
	case r := <-result:
		if r.err != nil {
			t.Errorf("unexpected err: %v", r.err)
		}
		if r.code != "code-xyz" {
			t.Errorf("code: got %q, want code-xyz", r.code)
		}
	default:
		t.Fatal("handler did not write to result channel")
	}
}

func TestCallbackHandler_StateMismatchRejected(t *testing.T) {
	result := make(chan callbackResult, 1)
	handler := callbackHandler("expected-state", result)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/callback?state=wrong&code=code-xyz", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
	r := <-result
	if r.err == nil || !strings.Contains(r.err.Error(), "state mismatch") {
		t.Errorf("expected state mismatch error, got %v", r.err)
	}
}

func TestCallbackHandler_TeslaErrorSurfaced(t *testing.T) {
	result := make(chan callbackResult, 1)
	handler := callbackHandler("expected-state", result)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/callback?error=access_denied&error_description=user+declined", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", rec.Code)
	}
	r := <-result
	if r.err == nil || !strings.Contains(r.err.Error(), "access_denied") {
		t.Errorf("expected access_denied error, got %v", r.err)
	}
}

func TestCallbackHandler_DuplicateSendDoesNotBlock(t *testing.T) {
	// Buffered capacity 1 — if the handler blocked on a second send, this
	// test would deadlock.
	result := make(chan callbackResult, 1)
	handler := callbackHandler("s", result)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/callback?state=s&code=c1", nil)
	handler(httptest.NewRecorder(), req)

	req2 := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/callback?state=s&code=c2", nil)
	// Must not block even though the channel is full. Use a timeout guard
	// so a regression surfaces as a test failure instead of a hang.
	done := make(chan struct{})
	go func() {
		handler(httptest.NewRecorder(), req2)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("duplicate callback blocked on full channel")
	}
}

func TestExchangeCodeForToken_SuccessParses200Response(t *testing.T) {
	var gotForm url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotForm, _ = url.ParseQuery(string(body))
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("content-type: got %q", ct)
		}
		// Literal JSON avoids the gosec G117 literal-struct credential scanner.
		_, _ = w.Write([]byte(`{"access_token":"fake-access","refresh_token":"fake-refresh","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer srv.Close()
	withTokenEndpoint(t, srv.URL)

	tok, err := exchangeCodeForToken(
		context.Background(),
		slog.Default(),
		"cid", "csec", "http://localhost:8765/callback", "the-code", "pkce-verifier",
	)
	if err != nil {
		t.Fatalf("exchangeCodeForToken: %v", err)
	}
	if tok.AccessToken != "fake-access" || tok.RefreshToken != "fake-refresh" {
		t.Errorf("token decode: %+v", tok)
	}
	if tok.ExpiresIn != 3600 {
		t.Errorf("expires_in: %d", tok.ExpiresIn)
	}
	// Confirm the real function built the expected form body.
	for k, want := range map[string]string{
		"grant_type":    "authorization_code",
		"client_id":     "cid",
		"client_secret": "csec",
		"code":          "the-code",
		"redirect_uri":  "http://localhost:8765/callback",
		"code_verifier": "pkce-verifier",
	} {
		if got := gotForm.Get(k); got != want {
			t.Errorf("form[%s]: got %q, want %q", k, got, want)
		}
	}
}

func TestExchangeCodeForToken_Non200ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
	}))
	defer srv.Close()
	withTokenEndpoint(t, srv.URL)

	_, err := exchangeCodeForToken(context.Background(), slog.Default(), "c", "s", "r", "c", "v")
	if err == nil {
		t.Fatal("expected error on 401")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error should include status + body, got: %v", err)
	}
}

func TestExchangeCodeForToken_MissingTokenFieldsRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"","refresh_token":"","expires_in":0}`))
	}))
	defer srv.Close()
	withTokenEndpoint(t, srv.URL)

	_, err := exchangeCodeForToken(context.Background(), slog.Default(), "c", "s", "r", "c", "v")
	if err == nil || !strings.Contains(err.Error(), "missing access_token") {
		t.Errorf("expected missing-token error, got: %v", err)
	}
}

// withTokenEndpoint points the Tesla token endpoint at a test server for
// the duration of a single test and restores the production URL at the end.
func withTokenEndpoint(t *testing.T, endpoint string) {
	t.Helper()
	prev := teslaOAuthTokenEndpoint
	teslaOAuthTokenEndpoint = endpoint
	t.Cleanup(func() { teslaOAuthTokenEndpoint = prev })
}
