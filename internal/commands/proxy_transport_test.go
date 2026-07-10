package commands

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProxyTransport_Enabled(t *testing.T) {
	if NewProxyTransport("", nil, nil).Enabled() {
		t.Fatalf("empty base URL must be disabled")
	}
	if !NewProxyTransport("https://proxy.local", nil, nil).Enabled() {
		t.Fatalf("configured base URL must be enabled")
	}
}

func TestProxyTransport_CommandResponseMapping(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   Outcome
	}{
		{"result true", 200, `{"response":{"result":true}}`, OutcomeOK},
		{"result false reason", 200, `{"response":{"result":false,"reason":"already_locked"}}`, OutcomeFailed},
		{"asleep 408", http.StatusRequestTimeout, `{"error":"vehicle unavailable"}`, OutcomeAsleep},
		{"asleep reason", 200, `{"response":{"result":false,"reason":"vehicle is not awake"}}`, OutcomeAsleep},
		{"not paired", 403, `{"error":"your key has not been paired with this vehicle"}`, OutcomeNotPaired},
		{"permission denied", 403, `{"error":"missing scope: not authorized"}`, OutcomePermissionDenied},
		{"counter error", 200, `{"response":{"result":false,"reason":"invalid signature: counter too low"}}`, OutcomeCounterError},
		{"invalid request", 400, `{"error":"invalid_command"}`, OutcomeInvalidRequest},
		{"server error", 500, `{"error":"internal"}`, OutcomeFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath, gotAuth, gotBody string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				b, _ := io.ReadAll(r.Body)
				gotBody = string(b)
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			tr := NewProxyTransport(srv.URL, srv.Client(), nil)
			res, err := tr.Command(context.Background(), TransportRequest{
				VIN: "5YJ3E1EA1PF000001", Command: "door_lock", Token: "abc", Body: []byte(`{"x":1}`),
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Outcome != tt.want {
				t.Fatalf("outcome = %v want %v", res.Outcome, tt.want)
			}
			if gotPath != "/api/1/vehicles/5YJ3E1EA1PF000001/command/door_lock" {
				t.Fatalf("path = %q", gotPath)
			}
			if gotAuth != "Bearer abc" {
				t.Fatalf("auth = %q", gotAuth)
			}
			if !strings.Contains(gotBody, `"x":1`) {
				t.Fatalf("body = %q", gotBody)
			}
		})
	}
}

func TestProxyTransport_Wake(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"response":{"state":"online"}}`))
	}))
	defer srv.Close()

	tr := NewProxyTransport(srv.URL, srv.Client(), nil)
	if err := tr.Wake(context.Background(), "5YJ3E1EA1PF000001", "abc"); err != nil {
		t.Fatalf("wake error: %v", err)
	}
	if gotPath != "/api/1/vehicles/5YJ3E1EA1PF000001/wake_up" {
		t.Fatalf("wake path = %q", gotPath)
	}
}

func TestProxyTransport_WakeNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestTimeout)
	}))
	defer srv.Close()
	tr := NewProxyTransport(srv.URL, srv.Client(), nil)
	if err := tr.Wake(context.Background(), "V", "abc"); err == nil {
		t.Fatalf("expected error on non-2xx wake")
	}
}
