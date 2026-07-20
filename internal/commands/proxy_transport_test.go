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
		name       string
		status     int
		body       string
		want       Outcome
		wantReason string
	}{
		{"result true", 200, `{"response":{"result":true}}`, OutcomeOK, ""},
		{"result false reason", 200, `{"response":{"result":false,"reason":"already_locked"}}`, OutcomeFailed, "already_locked"},
		{"asleep 408", http.StatusRequestTimeout, `{"error":"vehicle unavailable"}`, OutcomeAsleep, "vehicle unavailable"},
		{"asleep reason", 200, `{"response":{"result":false,"reason":"vehicle is not awake"}}`, OutcomeAsleep, "vehicle is not awake"},
		{"not paired", 403, `{"error":"your key has not been paired with this vehicle"}`, OutcomeNotPaired, "your key has not been paired with this vehicle"},
		{"permission denied", 403, `{"error":"missing scope: not authorized"}`, OutcomePermissionDenied, "missing scope: not authorized"},
		{"counter error", 200, `{"response":{"result":false,"reason":"invalid signature: counter too low"}}`, OutcomeCounterError, "invalid signature: counter too low"},
		{"invalid request", 400, `{"error":"invalid_command"}`, OutcomeInvalidRequest, "invalid_command"},
		// The exact proxy shape for an unknown command (navigation_gps_request):
		// ExtractCommandAction's default branch 400s with a null response and a
		// top-level `invalid_command` error. The reason must fall back to the
		// error string so the dispatch outcome can explain the rejection (MYR-245).
		{"invalid_command null response", 400, `{"response":null,"error":"invalid_command","error_description":""}`, OutcomeInvalidRequest, "invalid_command"},
		{"server error", 500, `{"error":"internal"}`, OutcomeFailed, "internal"},
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
			if res.Reason != tt.wantReason {
				t.Fatalf("reason = %q want %q", res.Reason, tt.wantReason)
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

func TestTruncateReason(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantLen int
	}{
		{"short passes through", "invalid_command", len("invalid_command")},
		{"empty stays empty", "", 0},
		{"long is clamped", strings.Repeat("a", 500), maxReasonLen},
		{"exactly at bound", strings.Repeat("b", maxReasonLen), maxReasonLen},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateReason(tt.in)
			if len([]rune(got)) != tt.wantLen {
				t.Errorf("len = %d want %d", len([]rune(got)), tt.wantLen)
			}
			if tt.wantLen <= maxReasonLen && got != "" && !strings.HasPrefix(tt.in, got) {
				t.Errorf("truncated %q is not a prefix of %q", got, tt.in)
			}
		})
	}
}

func TestClassifyResponse_ReasonFallbackAcrossBranches(t *testing.T) {
	// Every non-OK branch must surface a reason: envelope reason when present,
	// else the top-level error string (MYR-245).
	tests := []struct {
		name   string
		status int
		body   string
		want   Outcome
		reason string
	}{
		{"invalid_command via error fallback", 400, `{"response":null,"error":"invalid_command"}`, OutcomeInvalidRequest, "invalid_command"},
		{"not-paired via error fallback", 403, `{"error":"could not find a paired key"}`, OutcomeNotPaired, "could not find a paired key"},
		{"envelope reason preferred over error", 200, `{"response":{"result":false,"reason":"already_locked"},"error":"ignored"}`, OutcomeFailed, "already_locked"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res := classifyResponse(tt.status, []byte(tt.body))
			if res.Outcome != tt.want {
				t.Errorf("outcome = %v want %v", res.Outcome, tt.want)
			}
			if res.Reason != tt.reason {
				t.Errorf("reason = %q want %q", res.Reason, tt.reason)
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
