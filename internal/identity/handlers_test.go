package identity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestHandler(t *testing.T, store Store, apple appleTokenValidator, appleEnabled bool, perMin int) *Handler {
	t.Helper()
	ks, err := NewKeystoreFromPEM(testKeyPEM(t))
	if err != nil {
		t.Fatalf("keystore: %v", err)
	}
	svc := NewService(ServiceConfig{
		Store:      store,
		Apple:      apple,
		Minter:     NewTokenMinter(ks, "myrobotaxi", "telemetry", time.Hour),
		RefreshTTL: 90 * 24 * time.Hour,
	})
	return NewHandler(HandlerConfig{
		Service:            svc,
		Keystore:           ks,
		AppleEnabled:       appleEnabled,
		RateLimitPerMinute: perMin,
		RateLimitBurst:     perMin,
	})
}

func postJSON(t *testing.T, h http.HandlerFunc, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, strings.NewReader(body))
	req.RemoteAddr = "203.0.113.7:5555"
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func TestServeApple_Success(t *testing.T) {
	store := newFakeStore()
	store.prismaByEmail["owner@example.com"] = "cprismauser"
	h := newTestHandler(t, store, fakeApple{claims: AppleClaims{Sub: "s1", Email: "owner@example.com", EmailVerified: true}}, true, 100)

	rec := postJSON(t, h.ServeApple, "/api/auth/apple", `{"identityToken":"tok"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp authResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AccessToken == "" || resp.RefreshToken == "" || resp.ExpiresIn != 3600 {
		t.Errorf("bad response: %+v", resp)
	}
	if resp.User.ID != "cprismauser" {
		t.Errorf("user id = %q", resp.User.ID)
	}
}

func TestServeApple_Disabled404(t *testing.T) {
	h := newTestHandler(t, newFakeStore(), fakeApple{}, false, 100)
	rec := postJSON(t, h.ServeApple, "/api/auth/apple", `{"identityToken":"tok"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestServeApple_InvalidTokenIs401(t *testing.T) {
	h := newTestHandler(t, newFakeStore(), fakeApple{err: ErrInvalidAppleToken}, true, 100)
	rec := postJSON(t, h.ServeApple, "/api/auth/apple", `{"identityToken":"tok"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

func TestServeApple_BadBody(t *testing.T) {
	h := newTestHandler(t, newFakeStore(), fakeApple{}, true, 100)
	// Malformed JSON.
	if rec := postJSON(t, h.ServeApple, "/api/auth/apple", `{`); rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body status = %d, want 400", rec.Code)
	}
	// Missing required field.
	if rec := postJSON(t, h.ServeApple, "/api/auth/apple", `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("missing identityToken status = %d, want 400", rec.Code)
	}
	// Unknown field (strict decoding).
	if rec := postJSON(t, h.ServeApple, "/api/auth/apple", `{"identityToken":"x","evil":1}`); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown field status = %d, want 400", rec.Code)
	}
}

func TestServeRefresh(t *testing.T) {
	store := newFakeStore()
	store.rotateResult = RotateResult{Outcome: RotateRotated, UserID: "cuser"}
	h := newTestHandler(t, store, fakeApple{}, true, 100)

	rec := postJSON(t, h.ServeRefresh, "/api/auth/refresh", `{"refreshToken":"abc"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}

	store.rotateResult = RotateResult{Outcome: RotateReuse, UserID: "cuser", FamilyID: "f"}
	if rec := postJSON(t, h.ServeRefresh, "/api/auth/refresh", `{"refreshToken":"abc"}`); rec.Code != http.StatusUnauthorized {
		t.Errorf("reuse status = %d, want 401", rec.Code)
	}
	if rec := postJSON(t, h.ServeRefresh, "/api/auth/refresh", `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("missing token status = %d, want 400", rec.Code)
	}
}

func TestServeRevoke_204(t *testing.T) {
	store := newFakeStore()
	store.revokeFound = true
	store.revokeUserID = "cuser"
	h := newTestHandler(t, store, fakeApple{}, true, 100)
	rec := postJSON(t, h.ServeRevoke, "/api/auth/revoke", `{"refreshToken":"abc"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
}

func TestServeJWKS(t *testing.T) {
	h := newTestHandler(t, newFakeStore(), fakeApple{}, true, 100)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/auth/.well-known/jwks.json", http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeJWKS(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var jwks JWKS
	if err := json.Unmarshal(rec.Body.Bytes(), &jwks); err != nil {
		t.Fatalf("decode jwks: %v", err)
	}
	if len(jwks.Keys) != 1 || jwks.Keys[0].Kty != "EC" {
		t.Errorf("unexpected jwks: %+v", jwks)
	}
}

func TestServeRefresh_RateLimited(t *testing.T) {
	store := newFakeStore()
	store.rotateResult = RotateResult{Outcome: RotateRotated, UserID: "cuser"}
	h := newTestHandler(t, store, fakeApple{}, true, 1) // 1/min, burst 1

	// First request from the IP is allowed; the second is throttled.
	if rec := postJSON(t, h.ServeRefresh, "/api/auth/refresh", `{"refreshToken":"a"}`); rec.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", rec.Code)
	}
	rec := postJSON(t, h.ServeRefresh, "/api/auth/refresh", `{"refreshToken":"a"}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("missing Retry-After header on 429")
	}
}

// clientIP unit coverage for the XFF path.
func TestClientIP(t *testing.T) {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", http.NoBody)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.9, 10.0.0.1")
	if got := clientIP(req); got != "198.51.100.9" {
		t.Errorf("clientIP = %q, want leftmost XFF", got)
	}
	req2 := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/", http.NoBody)
	req2.RemoteAddr = "10.0.0.2:9999"
	if got := clientIP(req2); got != "10.0.0.2" {
		t.Errorf("clientIP = %q, want RemoteAddr host", got)
	}
}
