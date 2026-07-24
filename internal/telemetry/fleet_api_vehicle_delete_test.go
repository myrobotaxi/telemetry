package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFleetAPIClient_DeleteTelemetryConfig(t *testing.T) {
	t.Parallel()

	const (
		vin   = "5YJ3E1EA1PF000001"
		token = "tesla-oauth-token-abc" //nolint:gosec // test fixture, not a real credential
	)

	tests := []struct {
		name        string
		token       string
		vin         string
		status      int
		wantErr     bool
		wantReached bool // whether the request should reach the server
	}{
		{name: "success 200", token: token, vin: vin, status: http.StatusOK, wantErr: false, wantReached: true},
		{name: "already deleted 404 is an error (caller treats non-fatal)", token: token, vin: vin, status: http.StatusNotFound, wantErr: true, wantReached: true},
		{name: "server 500", token: token, vin: vin, status: http.StatusInternalServerError, wantErr: true, wantReached: true},
		{name: "empty token rejected before request", token: "", vin: vin, status: http.StatusOK, wantErr: true, wantReached: false},
		{name: "invalid VIN rejected before request", token: token, vin: "short", status: http.StatusOK, wantErr: true, wantReached: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var reached bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				if r.Method != http.MethodDelete {
					t.Errorf("method = %s, want DELETE", r.Method)
				}
				wantPath := "/api/1/vehicles/" + vin + "/fleet_telemetry_config"
				if r.URL.Path != wantPath {
					t.Errorf("path = %q, want %q", r.URL.Path, wantPath)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer "+token {
					t.Errorf("authorization = %q, want Bearer token", got)
				}
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			client := newTestFleetClient(srv.URL)
			err := client.DeleteTelemetryConfig(context.Background(), tt.token, tt.vin)

			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if reached != tt.wantReached {
				t.Errorf("server reached = %v, want %v", reached, tt.wantReached)
			}
		})
	}
}

// TestFleetAPIClient_DeleteTelemetryConfig_RedactsVINInError ensures the full
// VIN never appears in a returned error (only the last-4 redaction).
func TestFleetAPIClient_DeleteTelemetryConfig_RedactsVINInError(t *testing.T) {
	t.Parallel()
	const vin = "5YJ3E1EA1PF000042"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	err := newTestFleetClient(srv.URL).DeleteTelemetryConfig(context.Background(), "tok", vin)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), vin) {
		t.Errorf("error leaks full VIN: %q", err.Error())
	}
}
