package commands

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// hitRecorder is an httptest server that records whether it was called.
func hitRecorder(t *testing.T, body string) (*httptest.Server, *bool) {
	t.Helper()
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hit = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hit
}

// TestRoutingTransport_RoutesBySignerRequired proves the routing seam: a signed
// command reaches ONLY the proxy, an unsigned command reaches ONLY the Fleet
// REST base (MYR-245).
func TestRoutingTransport_RoutesBySignerRequired(t *testing.T) {
	tests := []struct {
		name           string
		signerRequired bool
		wantProxyHit   bool
		wantFleetHit   bool
	}{
		{"signed command routes to proxy", true, true, false},
		{"unsigned command routes to fleet REST", false, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxySrv, proxyHit := hitRecorder(t, `{"response":{"result":true}}`)
			fleetSrv, fleetHit := hitRecorder(t, `{"response":{"result":true}}`)

			rt := NewRoutingTransport(
				NewProxyTransport(proxySrv.URL, proxySrv.Client(), nil),
				NewFleetRESTTransport(fleetSrv.URL, fleetSrv.Client(), nil),
				nil,
			)
			res, err := rt.Command(context.Background(), TransportRequest{
				VIN: "V", Command: "cmd", Token: "t", SignerRequired: tt.signerRequired,
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Outcome != OutcomeOK {
				t.Fatalf("outcome = %v want OK", res.Outcome)
			}
			if *proxyHit != tt.wantProxyHit {
				t.Errorf("proxy hit = %v want %v", *proxyHit, tt.wantProxyHit)
			}
			if *fleetHit != tt.wantFleetHit {
				t.Errorf("fleet hit = %v want %v", *fleetHit, tt.wantFleetHit)
			}
		})
	}
}

// TestRoutingTransport_SignedWithoutProxyIsNotPaired proves a signed command
// with no proxy configured resolves to key_not_paired without dialing Fleet.
func TestRoutingTransport_SignedWithoutProxyIsNotPaired(t *testing.T) {
	fleetSrv, fleetHit := hitRecorder(t, `{"response":{"result":true}}`)
	rt := NewRoutingTransport(
		NewProxyTransport("", nil, nil), // no proxy
		NewFleetRESTTransport(fleetSrv.URL, fleetSrv.Client(), nil),
		nil,
	)
	res, err := rt.Command(context.Background(), TransportRequest{
		VIN: "V", Command: "door_lock", Token: "t", SignerRequired: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Outcome != OutcomeNotPaired {
		t.Fatalf("outcome = %v want OutcomeNotPaired", res.Outcome)
	}
	if *fleetHit {
		t.Fatalf("signed command must never dial the Fleet REST base")
	}
}

func TestRoutingTransport_EnabledReflectsProxyAndREST(t *testing.T) {
	tests := []struct {
		name        string
		proxyURL    string
		fleetURL    string
		wantEnabled bool
		wantREST    bool
	}{
		{"both configured", "https://proxy.local", "https://fleet.local", true, true},
		{"only fleet (no proxy)", "", "https://fleet.local", false, true},
		{"only proxy (no fleet)", "https://proxy.local", "", true, false},
		{"neither", "", "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rt := NewRoutingTransport(
				NewProxyTransport(tt.proxyURL, nil, nil),
				NewFleetRESTTransport(tt.fleetURL, nil, nil),
				nil,
			)
			if rt.Enabled() != tt.wantEnabled {
				t.Errorf("Enabled() = %v want %v", rt.Enabled(), tt.wantEnabled)
			}
			if rt.RESTEnabled() != tt.wantREST {
				t.Errorf("RESTEnabled() = %v want %v", rt.RESTEnabled(), tt.wantREST)
			}
		})
	}
}

// TestRoutingTransport_WakePrefersProxy proves Wake goes to the proxy when
// configured (signed path unchanged) and falls back to Fleet REST otherwise.
func TestRoutingTransport_WakePrefersProxy(t *testing.T) {
	t.Run("proxy configured wakes via proxy", func(t *testing.T) {
		proxySrv, proxyHit := hitRecorder(t, `{"response":{"state":"online"}}`)
		fleetSrv, fleetHit := hitRecorder(t, `{"response":{"state":"online"}}`)
		rt := NewRoutingTransport(
			NewProxyTransport(proxySrv.URL, proxySrv.Client(), nil),
			NewFleetRESTTransport(fleetSrv.URL, fleetSrv.Client(), nil),
			nil,
		)
		if err := rt.Wake(context.Background(), "V", "t"); err != nil {
			t.Fatalf("wake error: %v", err)
		}
		if !*proxyHit || *fleetHit {
			t.Fatalf("wake proxyHit=%v fleetHit=%v want proxy only", *proxyHit, *fleetHit)
		}
	})
	t.Run("no proxy wakes via fleet", func(t *testing.T) {
		fleetSrv, fleetHit := hitRecorder(t, `{"response":{"state":"online"}}`)
		rt := NewRoutingTransport(
			NewProxyTransport("", nil, nil),
			NewFleetRESTTransport(fleetSrv.URL, fleetSrv.Client(), nil),
			nil,
		)
		if err := rt.Wake(context.Background(), "V", "t"); err != nil {
			t.Fatalf("wake error: %v", err)
		}
		if !*fleetHit {
			t.Fatalf("expected fleet wake when no proxy configured")
		}
	})
}
