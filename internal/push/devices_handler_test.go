package push

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- fakes -----------------------------------------------------------------

type stubTokenValidator struct {
	userID string
	err    error
}

func (s *stubTokenValidator) ValidateToken(context.Context, string) (string, error) {
	return s.userID, s.err
}

// fakeRegistry records exactly what the handler handed the store, so the tests
// can prove the user id comes from the JWT and never from the request body.
type fakeRegistry struct {
	registerErr   error
	unregisterErr error
	removed       bool

	registerCalled   bool
	unregisterCalled bool
	gotUserID        string
	gotToken         string
	gotSandbox       bool
}

func (f *fakeRegistry) RegisterDevice(_ context.Context, userID, deviceToken string, sandbox bool) error {
	f.registerCalled = true
	f.gotUserID, f.gotToken, f.gotSandbox = userID, deviceToken, sandbox
	return f.registerErr
}

func (f *fakeRegistry) UnregisterDevice(_ context.Context, userID, deviceToken string) (bool, error) {
	f.unregisterCalled = true
	f.gotUserID, f.gotToken = userID, deviceToken
	return f.removed, f.unregisterErr
}

// --- helpers ---------------------------------------------------------------

const handlerUserID = "cuser-handler-001"

// doDeviceRequest mounts the handler on a real mux and performs one request.
func doDeviceRequest(
	t *testing.T,
	registry DeviceRegistry,
	auth tokenValidator,
	method, body string,
	withAuth bool,
) *httptest.ResponseRecorder {
	t.Helper()
	h := NewDevicesHandler(auth, registry, discardLogger())

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/push/devices", h.ServeRegister)
	mux.HandleFunc("DELETE /api/push/devices", h.ServeUnregister)

	req := httptest.NewRequestWithContext(context.Background(), method, "/api/push/devices", strings.NewReader(body))
	if withAuth {
		req.Header.Set("Authorization", "Bearer jwt")
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func okAuth() *stubTokenValidator { return &stubTokenValidator{userID: handlerUserID} }

// deviceErrorCode pulls `error.code` out of the §4.1 envelope.
func deviceErrorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("unmarshal error envelope: %v (body=%s)", err, rec.Body.String())
	}
	return env.Error.Code
}

// --- tests -----------------------------------------------------------------

// TestDevicesHandlerRegister covers the happy path and proves the store is
// handed the JWT-resolved user id, never anything from the request.
func TestDevicesHandlerRegister(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantToken   string
		wantSandbox bool
	}{
		{name: "production token", body: `{"deviceToken":"abc123"}`, wantToken: "abc123"},
		{name: "sandbox token", body: `{"deviceToken":"abc123","sandbox":true}`, wantToken: "abc123", wantSandbox: true},
		{name: "surrounding whitespace is trimmed", body: `{"deviceToken":"  abc123  "}`, wantToken: "abc123"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &fakeRegistry{}
			rec := doDeviceRequest(t, reg, okAuth(), http.MethodPut, tt.body, true)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}
			if !reg.registerCalled {
				t.Fatal("store was not called")
			}
			if reg.gotUserID != handlerUserID {
				t.Errorf("user id = %q, want the JWT subject %q", reg.gotUserID, handlerUserID)
			}
			if reg.gotToken != tt.wantToken {
				t.Errorf("token = %q, want %q", reg.gotToken, tt.wantToken)
			}
			if reg.gotSandbox != tt.wantSandbox {
				t.Errorf("sandbox = %v, want %v", reg.gotSandbox, tt.wantSandbox)
			}

			var got registerResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal response: %v", err)
			}
			if !got.Registered || got.Sandbox != tt.wantSandbox {
				t.Errorf("response = %+v, want registered with sandbox=%v", got, tt.wantSandbox)
			}
			// The token is P1 — it must not come back out in the response.
			if strings.Contains(rec.Body.String(), tt.wantToken) {
				t.Errorf("response %s echoes the P1 device token", rec.Body.String())
			}
		})
	}
}

// TestDevicesHandlerUnregister covers sign-out, including the idempotent miss.
func TestDevicesHandlerUnregister(t *testing.T) {
	tests := []struct {
		name    string
		removed bool
	}{
		{name: "removes the registration", removed: true},
		{name: "miss is still a 200", removed: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &fakeRegistry{removed: tt.removed}
			rec := doDeviceRequest(t, reg, okAuth(), http.MethodDelete, `{"deviceToken":"abc123"}`, true)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
			}
			if !reg.unregisterCalled {
				t.Fatal("store was not called")
			}
			if reg.gotUserID != handlerUserID {
				t.Errorf("user id = %q, want the JWT subject", reg.gotUserID)
			}

			var got unregisterResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal response: %v", err)
			}
			if got.Unregistered != tt.removed {
				t.Errorf("unregistered = %v, want %v", got.Unregistered, tt.removed)
			}
		})
	}
}

// TestDevicesHandlerRejects covers every 4xx/5xx branch across both verbs.
func TestDevicesHandlerRejects(t *testing.T) {
	longToken := strings.Repeat("a", maxDeviceTokenLen+1)

	tests := []struct {
		name        string
		method      string
		body        string
		withAuth    bool
		auth        *stubTokenValidator
		registry    *fakeRegistry
		wantStatus  int
		wantCode    string
		wantNoWrite bool
	}{
		{
			name: "missing authorization", method: http.MethodPut, body: `{"deviceToken":"abc"}`,
			auth: okAuth(), wantStatus: http.StatusUnauthorized, wantCode: "auth_failed", wantNoWrite: true,
		},
		{
			name: "invalid token", method: http.MethodPut, body: `{"deviceToken":"abc"}`, withAuth: true,
			auth:       &stubTokenValidator{err: errors.New("expired")},
			wantStatus: http.StatusUnauthorized, wantCode: "auth_failed", wantNoWrite: true,
		},
		{
			name: "malformed json", method: http.MethodPut, body: `{`, withAuth: true,
			auth: okAuth(), wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantNoWrite: true,
		},
		{
			name: "unknown field", method: http.MethodPut, body: `{"deviceToken":"abc","platform":"android"}`, withAuth: true,
			auth: okAuth(), wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantNoWrite: true,
		},
		{
			name: "empty token", method: http.MethodPut, body: `{"deviceToken":"   "}`, withAuth: true,
			auth: okAuth(), wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantNoWrite: true,
		},
		{
			name: "missing token key", method: http.MethodPut, body: `{}`, withAuth: true,
			auth: okAuth(), wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantNoWrite: true,
		},
		{
			name: "oversized token", method: http.MethodPut, body: `{"deviceToken":"` + longToken + `"}`, withAuth: true,
			auth: okAuth(), wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantNoWrite: true,
		},
		{
			name: "token with embedded whitespace", method: http.MethodPut, body: `{"deviceToken":"abc def"}`, withAuth: true,
			auth: okAuth(), wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantNoWrite: true,
		},
		{
			name: "store failure on register", method: http.MethodPut, body: `{"deviceToken":"abc"}`, withAuth: true,
			auth: okAuth(), registry: &fakeRegistry{registerErr: errors.New("db down")},
			wantStatus: http.StatusInternalServerError, wantCode: "internal_error",
		},
		{
			name: "delete rejects a bad body too", method: http.MethodDelete, body: `{"deviceToken":""}`, withAuth: true,
			auth: okAuth(), wantStatus: http.StatusBadRequest, wantCode: "invalid_request", wantNoWrite: true,
		},
		{
			name: "store failure on unregister", method: http.MethodDelete, body: `{"deviceToken":"abc"}`, withAuth: true,
			auth: okAuth(), registry: &fakeRegistry{unregisterErr: errors.New("db down")},
			wantStatus: http.StatusInternalServerError, wantCode: "internal_error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := tt.registry
			if reg == nil {
				reg = &fakeRegistry{}
			}
			rec := doDeviceRequest(t, reg, tt.auth, tt.method, tt.body, tt.withAuth)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if got := deviceErrorCode(t, rec); got != tt.wantCode {
				t.Errorf("error.code = %q, want %q", got, tt.wantCode)
			}
			if tt.wantNoWrite && (reg.registerCalled || reg.unregisterCalled) {
				t.Error("store was called despite a rejected request")
			}
			// A rejected token must never be echoed back — it is P1.
			if strings.Contains(rec.Body.String(), longToken) {
				t.Error("error envelope echoes the rejected device token")
			}
		})
	}
}

// TestDevicesHandlerMethodNotAllowed pins the 405s. Each ServeX enforces its
// own verb, so a future mux mistake surfaces as a 405 rather than the wrong
// operation running.
func TestDevicesHandlerMethodNotAllowed(t *testing.T) {
	tests := []struct {
		name   string
		serve  func(h *DevicesHandler) http.HandlerFunc
		method string
	}{
		{name: "register rejects POST", serve: func(h *DevicesHandler) http.HandlerFunc { return h.ServeRegister }, method: http.MethodPost},
		{name: "unregister rejects PUT", serve: func(h *DevicesHandler) http.HandlerFunc { return h.ServeUnregister }, method: http.MethodPut},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &fakeRegistry{}
			h := NewDevicesHandler(okAuth(), reg, discardLogger())
			req := httptest.NewRequestWithContext(context.Background(), tt.method, "/api/push/devices", strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer jwt")
			rec := httptest.NewRecorder()

			tt.serve(h)(rec, req)

			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d, want 405", rec.Code)
			}
			if reg.registerCalled || reg.unregisterCalled {
				t.Error("store was called on a rejected method")
			}
		})
	}
}

func TestValidateDeviceToken(t *testing.T) {
	tests := []struct {
		name    string
		token   string
		wantErr bool
	}{
		{name: "typical apns token", token: strings.Repeat("a1b2", 16)},
		{name: "at the length cap", token: strings.Repeat("a", maxDeviceTokenLen)},
		{name: "empty", token: "", wantErr: true},
		{name: "over the cap", token: strings.Repeat("a", maxDeviceTokenLen+1), wantErr: true},
		{name: "embedded newline", token: "abc\ndef", wantErr: true},
		{name: "embedded tab", token: "abc\tdef", wantErr: true},
		{name: "embedded null", token: "abc\x00def", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := validateDeviceToken(tt.token)
			if got := msg != ""; got != tt.wantErr {
				t.Errorf("validateDeviceToken() rejected = %v (%q), want %v", got, msg, tt.wantErr)
			}
			if msg != "" && strings.Contains(msg, tt.token) && tt.token != "" {
				t.Errorf("rejection message %q echoes the token", msg)
			}
		})
	}
}

func TestBearerToken(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{name: "bearer token", header: "Bearer abc.def", want: "abc.def"},
		{name: "missing header"},
		{name: "wrong scheme", header: "Basic abc"},
		{name: "lowercase scheme is rejected", header: "bearer abc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, "/api/push/devices", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			if got := bearerToken(req); got != tt.want {
				t.Errorf("bearerToken() = %q, want %q", got, tt.want)
			}
		})
	}
}
