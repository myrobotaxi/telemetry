package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFleetAPIClient_GetVehicle(t *testing.T) {
	t.Parallel()

	const (
		vin   = "7SAYGDET7TA613795"
		token = "tesla-oauth-token-abc" //nolint:gosec // test fixture, not a real credential
	)

	tests := []struct {
		name          string
		token         string
		vin           string
		status        int
		body          string
		wantErr       bool
		wantReached   bool
		wantInService bool
		wantState     string
	}{
		{
			name:          "online in service",
			token:         token,
			vin:           vin,
			status:        http.StatusOK,
			body:          `{"response":{"vin":"` + vin + `","state":"online","in_service":true}}`,
			wantReached:   true,
			wantInService: true,
			wantState:     "online",
		},
		{
			name:        "offline not in service",
			token:       token,
			vin:         vin,
			status:      http.StatusOK,
			body:        `{"response":{"vin":"` + vin + `","state":"offline","in_service":false}}`,
			wantReached: true,
			wantState:   "offline",
		},
		{name: "server 500", token: token, vin: vin, status: http.StatusInternalServerError, body: "", wantErr: true, wantReached: true},
		{name: "empty token rejected before request", token: "", vin: vin, status: http.StatusOK, wantErr: true, wantReached: false},
		{name: "invalid VIN rejected before request", token: token, vin: "short", status: http.StatusOK, wantErr: true, wantReached: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var reached bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				if r.Method != http.MethodGet {
					t.Errorf("method = %s, want GET", r.Method)
				}
				wantPath := "/api/1/vehicles/" + vin
				if r.URL.Path != wantPath {
					t.Errorf("path = %q, want %q", r.URL.Path, wantPath)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer "+token {
					t.Errorf("authorization = %q, want Bearer token", got)
				}
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			client := newTestFleetClient(srv.URL)
			got, err := client.GetVehicle(context.Background(), tt.token, tt.vin)

			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if reached != tt.wantReached {
				t.Errorf("server reached = %v, want %v", reached, tt.wantReached)
			}
			if tt.wantErr {
				return
			}
			if got.InService != tt.wantInService {
				t.Errorf("InService = %v, want %v", got.InService, tt.wantInService)
			}
			if got.State != tt.wantState {
				t.Errorf("State = %q, want %q", got.State, tt.wantState)
			}
		})
	}
}

// TestFleetAPIClient_GetVehicle_RedactsVINInError ensures the full VIN never
// appears in a returned error (only the last-4 redaction).
func TestFleetAPIClient_GetVehicle_RedactsVINInError(t *testing.T) {
	t.Parallel()

	const vin = "7SAYGDET7TA613795"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	_, err := newTestFleetClient(srv.URL).GetVehicle(context.Background(), "tok", vin)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), vin) {
		t.Errorf("error leaks full VIN: %v", err)
	}
}
