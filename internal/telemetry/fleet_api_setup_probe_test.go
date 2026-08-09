package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	probeVIN   = "7SAYGDET7TA613795"
	probeToken = "tesla-oauth-token-abc" //nolint:gosec // test fixture, not a real credential
)

// TestFleetAPIClient_GetFleetStatus pins the request shape and, above all, the
// POSITIVE-MEMBERSHIP reading of the response: a VIN Tesla mentions in neither
// list must read as UNPAIRED, because a false positive here authorises a
// delete-then-create against a real customer car.
func TestFleetAPIClient_GetFleetStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		token      string
		vins       []string
		status     int
		body       string
		wantErr    bool
		wantCall   bool
		wantPaired bool
	}{
		{
			name: "paired", token: probeToken, vins: []string{probeVIN}, status: http.StatusOK,
			body:     `{"response":{"key_paired_vins":["` + probeVIN + `"],"unpaired_vins":[]}}`,
			wantCall: true, wantPaired: true,
		},
		{
			name: "explicitly unpaired", token: probeToken, vins: []string{probeVIN}, status: http.StatusOK,
			body:     `{"response":{"key_paired_vins":[],"unpaired_vins":["` + probeVIN + `"]}}`,
			wantCall: true,
		},
		{
			// The dangerous one. Tesla answered, but said nothing about our
			// VIN — an unknown car, or a token that cannot see it. "Absent
			// from unpaired_vins" must NOT be read as paired.
			name: "absent from both lists is not paired", token: probeToken, vins: []string{probeVIN},
			status:   http.StatusOK,
			body:     `{"response":{"key_paired_vins":["7SAYGDED5TA999274"],"unpaired_vins":[]}}`,
			wantCall: true,
		},
		{
			// A different car being paired says nothing about ours.
			name: "another VIN paired is not ours", token: probeToken, vins: []string{probeVIN},
			status:   http.StatusOK,
			body:     `{"response":{"key_paired_vins":[],"unpaired_vins":["7SAYGDED5TA999274"]}}`,
			wantCall: true,
		},
		{
			name: "empty response object", token: probeToken, vins: []string{probeVIN},
			status: http.StatusOK, body: `{"response":{}}`, wantCall: true,
		},
		{
			name: "missing token never dials", vins: []string{probeVIN}, wantErr: true,
		},
		{
			name: "no VINs never dials", token: probeToken, wantErr: true,
		},
		{
			name: "short VIN never dials", token: probeToken, vins: []string{"TOOSHORT"}, wantErr: true,
		},
		{
			name: "upstream 403", token: probeToken, vins: []string{probeVIN},
			status: http.StatusForbidden, body: `{"error":"unauthorized"}`,
			wantErr: true, wantCall: true,
		},
		{
			name: "undecodable body", token: probeToken, vins: []string{probeVIN},
			status: http.StatusOK, body: `not json`, wantErr: true, wantCall: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var reached bool
			var gotPath, gotAuth string
			var gotBody fleetStatusRequest

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &gotBody)
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			c := NewFleetAPIClient(FleetAPIConfig{BaseURL: srv.URL, MaxRetries: 1}, fleetTestLogger())
			resp, err := c.GetFleetStatus(context.Background(), tt.token, tt.vins)

			if reached != tt.wantCall {
				t.Fatalf("server reached = %v, want %v", reached, tt.wantCall)
			}
			if tt.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				// A VIN must never survive into an error string (§9, CG-DC-2).
				if strings.Contains(err.Error(), probeVIN) {
					t.Errorf("error leaks the full VIN: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("GetFleetStatus: %v", err)
			}
			if got := resp.KeyPaired(probeVIN); got != tt.wantPaired {
				t.Errorf("KeyPaired = %v, want %v", got, tt.wantPaired)
			}
			if gotPath != "/api/1/vehicles/fleet_status" {
				t.Errorf("path = %q, want /api/1/vehicles/fleet_status", gotPath)
			}
			if gotAuth != "Bearer "+probeToken {
				t.Errorf("Authorization = %q, want the bearer token", gotAuth)
			}
			if len(gotBody.VINs) != 1 || gotBody.VINs[0] != probeVIN {
				t.Errorf("request vins = %v, want [%s]", gotBody.VINs, probeVIN)
			}
		})
	}
}

// TestFleetStatusKeyPairedNilReceiver: a nil response must read as NOT paired.
// The push path is guarded by this answer, so the nil case has to fail closed.
func TestFleetStatusKeyPairedNilReceiver(t *testing.T) {
	t.Parallel()
	var resp *FleetStatusResponse
	if resp.KeyPaired(probeVIN) {
		t.Error("a nil FleetStatusResponse reported paired; it must fail closed")
	}
}

// TestFleetAPIClient_WakeVehicle pins the verb, the path, and the contract that
// a successful wake reports Tesla's ACCEPTANCE — the returned `asleep` state is
// the pre-wake one and must not be mistaken for a failure.
func TestFleetAPIClient_WakeVehicle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		token     string
		vin       string
		status    int
		body      string
		wantErr   bool
		wantCall  bool
		wantState string
	}{
		{
			name: "accepted while still asleep", token: probeToken, vin: probeVIN, status: http.StatusOK,
			body:     `{"response":{"vin":"` + probeVIN + `","state":"asleep"}}`,
			wantCall: true, wantState: "asleep",
		},
		{
			name: "already online", token: probeToken, vin: probeVIN, status: http.StatusOK,
			body:     `{"response":{"vin":"` + probeVIN + `","state":"online"}}`,
			wantCall: true, wantState: "online",
		},
		{
			name: "missing token never dials", vin: probeVIN, wantErr: true,
		},
		{
			name: "short VIN never dials", token: probeToken, vin: "TOOSHORT", wantErr: true,
		},
		{
			name: "upstream 408", token: probeToken, vin: probeVIN,
			status: http.StatusRequestTimeout, body: `{"error":"vehicle unavailable"}`,
			wantErr: true, wantCall: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var reached bool
			var gotMethod, gotPath string

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				gotMethod = r.Method
				gotPath = r.URL.Path
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			c := NewFleetAPIClient(FleetAPIConfig{BaseURL: srv.URL, MaxRetries: 0}, fleetTestLogger())
			state, err := c.WakeVehicle(context.Background(), tt.token, tt.vin)

			if reached != tt.wantCall {
				t.Fatalf("server reached = %v, want %v", reached, tt.wantCall)
			}
			if tt.wantErr {
				if err == nil {
					t.Fatal("want error, got nil")
				}
				if strings.Contains(err.Error(), probeVIN) {
					t.Errorf("error leaks the full VIN: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("WakeVehicle: %v", err)
			}
			if gotMethod != http.MethodPost {
				t.Errorf("method = %s, want POST", gotMethod)
			}
			if want := "/api/1/vehicles/" + probeVIN + "/wake_up"; gotPath != want {
				t.Errorf("path = %q, want %q", gotPath, want)
			}
			if state.State != tt.wantState {
				t.Errorf("state = %q, want %q", state.State, tt.wantState)
			}
		})
	}
}
