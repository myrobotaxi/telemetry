package commands

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFleetRESTTransport_Enabled(t *testing.T) {
	if NewFleetRESTTransport("", nil, nil).Enabled() {
		t.Fatalf("empty base URL must be disabled")
	}
	if !NewFleetRESTTransport("https://fleet-api.prd.na.vn.cloud.tesla.com", nil, nil).Enabled() {
		t.Fatalf("configured base URL must be enabled")
	}
}

// TestFleetRESTTransport_CommandRoutesToFleet proves an unsigned command POSTs
// to the Fleet API base at /api/1/vehicles/{vin}/command/{name} with the
// owner's bearer token and the exact body, and that the shared classifier maps
// each Fleet REST status the same way it maps the proxy's (MYR-245).
func TestFleetRESTTransport_CommandRoutesToFleet(t *testing.T) {
	const navBody = `{"type":"share_ext_content_raw","value":{"android.intent.extra.TEXT":"33.0,-96.8"}}`
	tests := []struct {
		name       string
		status     int
		body       string
		want       Outcome
		wantReason string
	}{
		// The live-verified success shape: 200 result:true (queued:true) → OK.
		{"200 result true queued", http.StatusOK, `{"response":{"result":true,"queued":true}}`, OutcomeOK, ""},
		// Fleet REST returns 408 when the vehicle is asleep → asleep (wake+retry).
		{"408 vehicle unavailable asleep", http.StatusRequestTimeout, `{"response":null,"error":"vehicle unavailable"}`, OutcomeAsleep, "vehicle unavailable"},
		{"401 unauthorized", http.StatusUnauthorized, `{"error":"token expired"}`, OutcomePermissionDenied, "token expired"},
		{"403 forbidden scope", http.StatusForbidden, `{"error":"missing scope"}`, OutcomePermissionDenied, "missing scope"},
		{"400 invalid request reason", http.StatusBadRequest, `{"error":"invalid_command"}`, OutcomeInvalidRequest, "invalid_command"},
		{"422 unprocessable", http.StatusUnprocessableEntity, `{"error":"invalid parameter"}`, OutcomeInvalidRequest, "invalid parameter"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotPath, gotAuth, gotBody, gotMethod string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotMethod = r.Method
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				b, _ := io.ReadAll(r.Body)
				gotBody = string(b)
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			tr := NewFleetRESTTransport(srv.URL, srv.Client(), nil)
			res, err := tr.Command(context.Background(), TransportRequest{
				VIN: "5YJ3E1EA1PF000001", Command: "navigation_request", Token: "owner-bearer",
				Body: []byte(navBody), SignerRequired: false,
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
			if gotMethod != http.MethodPost {
				t.Fatalf("method = %q want POST", gotMethod)
			}
			if gotPath != "/api/1/vehicles/5YJ3E1EA1PF000001/command/navigation_request" {
				t.Fatalf("path = %q", gotPath)
			}
			if gotAuth != "Bearer owner-bearer" {
				t.Fatalf("auth = %q", gotAuth)
			}
			if gotBody != navBody {
				t.Fatalf("body = %q want %q", gotBody, navBody)
			}
		})
	}
}

func TestFleetRESTTransport_Wake(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"response":{"state":"online"}}`))
	}))
	defer srv.Close()

	tr := NewFleetRESTTransport(srv.URL, srv.Client(), nil)
	if err := tr.Wake(context.Background(), "5YJ3E1EA1PF000001", "abc"); err != nil {
		t.Fatalf("wake error: %v", err)
	}
	if gotPath != "/api/1/vehicles/5YJ3E1EA1PF000001/wake_up" {
		t.Fatalf("wake path = %q", gotPath)
	}
}

func TestFleetRESTTransport_WakeNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestTimeout)
	}))
	defer srv.Close()
	tr := NewFleetRESTTransport(srv.URL, srv.Client(), nil)
	if err := tr.Wake(context.Background(), "V", "abc"); err == nil {
		t.Fatalf("expected error on non-2xx wake")
	}
}

// TestFleetRESTTransport_TrimsTrailingSlash guards the base-URL join so a
// configured "https://host/" does not produce a "//api" double slash.
func TestFleetRESTTransport_TrimsTrailingSlash(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"response":{"result":true}}`))
	}))
	defer srv.Close()

	tr := NewFleetRESTTransport(srv.URL+"/", srv.Client(), nil)
	if _, err := tr.Command(context.Background(), TransportRequest{VIN: "V", Command: "navigation_request", Token: "t"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(gotPath, "//") {
		t.Fatalf("path has double slash: %q", gotPath)
	}
}
