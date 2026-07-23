package teslalink

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/teslaauth"
)

type fakeValidator struct {
	userID string
	err    error
}

func (f fakeValidator) ValidateToken(_ context.Context, _ string) (string, error) {
	return f.userID, f.err
}

type fakeLinker struct {
	gotUserID string
	err       error
}

func (f *fakeLinker) UpdateTeslaToken(_ context.Context, userID, _, _ string, _ int64) error {
	f.gotUserID = userID
	return f.err
}

func testConfig() Config {
	return Config{
		ClientID:       "cid",
		ClientSecret:   "csec",
		RedirectURI:    "https://telemetry.myrobotaxi.app/api/tesla/link/callback",
		AppRedirectURL: "myrobotaxi://tesla-linked",
	}
}

func newTestHandler(auth tokenValidator, linker AccountLinker) (*Handler, *SessionStore) {
	store := NewSessionStore(10 * time.Minute)
	h := NewHandler(auth, linker, store, testConfig(), slog.Default())
	return h, store
}

func TestServeStart(t *testing.T) {
	tests := []struct {
		name       string
		authHeader string
		validator  fakeValidator
		wantStatus int
	}{
		{name: "missing bearer", authHeader: "", validator: fakeValidator{userID: "u1"}, wantStatus: http.StatusUnauthorized},
		{name: "invalid token", authHeader: "Bearer bad", validator: fakeValidator{err: errors.New("nope")}, wantStatus: http.StatusUnauthorized},
		{name: "success", authHeader: "Bearer good", validator: fakeValidator{userID: "u1"}, wantStatus: http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h, store := newTestHandler(tt.validator, &fakeLinker{})
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/tesla/link/start", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()
			h.ServeStart(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status: got %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantStatus != http.StatusOK {
				if store.Len() != 0 {
					t.Errorf("no session should be stored on failure, got %d", store.Len())
				}
				return
			}
			// Success: one session stored; response carries a valid authorize URL
			// with prompt_missing_scopes and a state that matches the store key.
			if store.Len() != 1 {
				t.Fatalf("expected 1 stored session, got %d", store.Len())
			}
			body := rec.Body.String()
			if !strings.Contains(body, "auth.tesla.com/oauth2/v3/authorize") {
				t.Errorf("missing authorize URL: %s", body)
			}
			if !strings.Contains(body, "prompt_missing_scopes") {
				t.Errorf("authorize URL must request prompt_missing_scopes: %s", body)
			}
		})
	}
}

func TestResolveCallback(t *testing.T) {
	okExchange := func(_ context.Context, _ *slog.Logger, _, _, _, _, _ string) (*teslaauth.TokenResponse, error) {
		return &teslaauth.TokenResponse{AccessToken: "a", RefreshToken: "r", ExpiresIn: 3600}, nil
	}
	failExchange := func(_ context.Context, _ *slog.Logger, _, _, _, _, _ string) (*teslaauth.TokenResponse, error) {
		return nil, errors.New("tesla returned 400")
	}

	tests := []struct {
		name       string
		seedState  string // state stored before the callback ("" = none)
		query      url.Values
		exchange   exchangeFunc
		linkerErr  error
		wantStatus string
		wantReason string
		wantLinked bool
	}{
		{
			name:       "tesla denied",
			query:      url.Values{"error": {"access_denied"}, "state": {"s1"}},
			seedState:  "s1",
			exchange:   okExchange,
			wantStatus: statusError,
			wantReason: reasonTeslaDenied,
		},
		{
			name:       "invalid state (no session)",
			query:      url.Values{"state": {"unknown"}, "code": {"c"}},
			exchange:   okExchange,
			wantStatus: statusError,
			wantReason: reasonInvalidState,
		},
		{
			name:       "missing code",
			seedState:  "s1",
			query:      url.Values{"state": {"s1"}},
			exchange:   okExchange,
			wantStatus: statusError,
			wantReason: reasonMissingCode,
		},
		{
			name:       "exchange failed",
			seedState:  "s1",
			query:      url.Values{"state": {"s1"}, "code": {"c"}},
			exchange:   failExchange,
			wantStatus: statusError,
			wantReason: reasonExchangeFailed,
		},
		{
			name:       "account not provisioned",
			seedState:  "s1",
			query:      url.Values{"state": {"s1"}, "code": {"c"}},
			exchange:   okExchange,
			linkerErr:  ErrAccountNotProvisioned,
			wantStatus: statusError,
			wantReason: reasonNotProvisioned,
		},
		{
			name:       "persist failed",
			seedState:  "s1",
			query:      url.Values{"state": {"s1"}, "code": {"c"}},
			exchange:   okExchange,
			linkerErr:  errors.New("db down"),
			wantStatus: statusError,
			wantReason: reasonPersistFailed,
		},
		{
			name:       "success",
			seedState:  "s1",
			query:      url.Values{"state": {"s1"}, "code": {"c"}},
			exchange:   okExchange,
			wantStatus: statusSuccess,
			wantLinked: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			linker := &fakeLinker{err: tt.linkerErr}
			h, store := newTestHandler(fakeValidator{userID: "u1"}, linker)
			h.exchange = tt.exchange
			if tt.seedState != "" {
				store.Put(Session{State: tt.seedState, PKCEVerifier: "v1", UserID: "u1"})
			}

			res := h.resolveCallback(context.Background(), tt.query)
			if res.status != tt.wantStatus || res.reason != tt.wantReason {
				t.Fatalf("outcome: got {%s %s}, want {%s %s}", res.status, res.reason, tt.wantStatus, tt.wantReason)
			}
			if tt.wantLinked && linker.gotUserID != "u1" {
				t.Errorf("expected token persisted for u1, got %q", linker.gotUserID)
			}
			// The state session must be consumed once Take is reached with a match.
			// tesla_denied returns before Take (error param wins), so it is exempt.
			if tt.seedState != "" && tt.wantReason != reasonInvalidState && tt.wantReason != reasonTeslaDenied {
				if _, ok := store.Take(tt.seedState); ok {
					t.Error("session should have been consumed (single-use)")
				}
			}
		})
	}
}

func TestServeCallback_RedirectsToApp(t *testing.T) {
	linker := &fakeLinker{}
	h, store := newTestHandler(fakeValidator{userID: "u1"}, linker)
	h.exchange = func(_ context.Context, _ *slog.Logger, _, _, _, _, _ string) (*teslaauth.TokenResponse, error) {
		return &teslaauth.TokenResponse{AccessToken: "a", RefreshToken: "r", ExpiresIn: 3600}, nil
	}
	store.Put(Session{State: "s1", PKCEVerifier: "v1", UserID: "u1"})

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/tesla/link/callback?state=s1&code=c", nil)
	rec := httptest.NewRecorder()
	h.ServeCallback(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "myrobotaxi://tesla-linked?") || !strings.Contains(loc, "status=success") {
		t.Errorf("unexpected redirect location: %q", loc)
	}
	// Tokens must NEVER appear in the redirect URL.
	if strings.Contains(loc, "access") || strings.Contains(loc, "a") && strings.Contains(loc, "token") {
		t.Errorf("redirect leaked token material: %q", loc)
	}
}

func TestAppRedirectURL(t *testing.T) {
	tests := []struct {
		name   string
		base   string
		status string
		reason string
		want   string
	}{
		{name: "success", base: "myrobotaxi://tesla-linked", status: "success", reason: "", want: "myrobotaxi://tesla-linked?status=success"},
		{name: "error with reason", base: "myrobotaxi://tesla-linked", status: "error", reason: "invalid_state", want: "myrobotaxi://tesla-linked?reason=invalid_state&status=error"},
		{name: "base already has query", base: "myrobotaxi://tesla-linked?x=1", status: "success", reason: "", want: "myrobotaxi://tesla-linked?x=1&status=success"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := appRedirectURL(tt.base, tt.status, tt.reason); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}
