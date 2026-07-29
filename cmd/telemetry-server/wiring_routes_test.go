package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/server"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
	"github.com/myrobotaxi/telemetry/internal/ws"
)

// alwaysReady is a server.ReadinessChecker that reports healthy. The
// route-surface test never hits /readyz, but server.New requires a
// non-nil checker.
type alwaysReady struct{}

func (alwaysReady) Ping(context.Context) error { return nil }

// TestSetupHTTPHandlers_RouteSurface is a regression guard for the
// DV-20 "phantom endpoint" class of bug (MYR-130): an SDK-contract REST
// route that no handler is registered for returns 404, which the SDK
// surfaces as a confusing not_found instead of the expected auth
// challenge. It wires the real composition root (setupHTTPHandlers) with
// minimal deps — nil repos are fine because the handlers only store them;
// no request in this test reaches the store layer, since every request
// is unauthenticated and fails at the bearer-token gate first — then
// asserts that each contract route is MOUNTED: an unauthenticated GET
// must return something OTHER than 404 (401/400 prove the route exists).
//
// This test FAILS on origin/main (GET /api/drives/{driveId} 404s because
// no handler is registered) and PASSES once the drive-detail handler is
// wired in setupHTTPHandlers.
func TestSetupHTTPHandlers_RouteSurface(t *testing.T) {
	logger := testLogger()

	// Zero-value config: WebSocket() yields a zero WriteTimeout and
	// Proxy().URL is empty, so setupFleetConfigEndpoint early-returns and
	// the debug-fields gate stays disabled. No ports are bound — we never
	// call Start.
	cfg := &config.Config{}

	srv := server.New(config.ServerConfig{}, logger, alwaysReady{}, prometheus.NewRegistry(), "")

	bus := events.NewChannelBus(events.DefaultBusConfig(), events.NoopBusMetrics{}, logger)
	recv := telemetry.NewReceiver(
		telemetry.NewDecoder(),
		bus,
		logger,
		telemetry.NoopReceiverMetrics{},
		telemetry.ReceiverConfig{},
	)
	hub := ws.NewHub(logger, ws.NoopHubMetrics{})

	deps := httpRouteDeps{
		cfg:           cfg,
		srv:           srv,
		hub:           hub,
		authenticator: &ws.NoopAuthenticator{},
		recv:          recv,
		bus:           bus,
		// Repos are nil: the handlers store the pointer at construction
		// and only dereference it inside a request that has already
		// passed auth. Every request below is unauthenticated, so no
		// store call is ever made.
		logger: logger,
	}

	setupHTTPHandlers(deps)

	handler := srv.ClientHandler()

	// The SDK-contract REST route surface (rest-api.md §6 / §7). Concrete
	// path values substitute the {…} wildcards. Every one MUST be
	// mounted; a 404 means the route is missing (the MYR-130 bug class).
	routes := []struct {
		name string
		path string
	}{
		{"vehicles list (§7.0)", "/api/vehicles"},
		{"vehicle snapshot (§7.1)", "/api/vehicles/clxyz1234567890abcdef/snapshot"},
		{"vehicle drives (§7.2)", "/api/vehicles/clxyz1234567890abcdef/drives"},
		{"drive detail (§7.3)", "/api/drives/clmno9876543210zyxw0001"},
		{"drive route (§7.4)", "/api/drives/clmno9876543210zyxw0001/route"},
		{"vehicle status", "/api/vehicle-status/5YJ3E1EA1PF000001"},
		// MYR-174: rider-facing ride-request surface. GET routes are
		// exercised here (an unauthenticated GET must not 404); the POST
		// create/cancel routes are asserted separately below.
		{"ride requests list (MYR-174)", "/api/ride-requests"},
		{"ride request detail (MYR-174)", "/api/ride-requests/crr0123456789abcdef0123456789abcd"},
		{"ride requests incoming feed (MYR-175)", "/api/ride-requests/incoming"},
		// MYR-184 vehicle sharing (§7.5). The owner's invite list.
		{"share invite list (MYR-184, §7.5)", "/api/vehicles/clxyz1234567890abcdef/invites"},
	}

	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, rt.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code == http.StatusNotFound {
				t.Fatalf("route %q returned 404 — handler not mounted. Body: %s", rt.path, rec.Body.String())
			}
		})
	}

	// MYR-174 POST routes: create + cancel. An unauthenticated POST must
	// fail the bearer gate (401), never 404 (unmounted).
	postRoutes := []struct {
		name string
		path string
	}{
		{"ride request create (MYR-174)", "/api/ride-requests"},
		{"ride request cancel (MYR-174)", "/api/ride-requests/crr0123456789abcdef0123456789abcd/cancel"},
		{"ride request picked-up (MYR-270)", "/api/ride-requests/crr0123456789abcdef0123456789abcd/picked-up"},
		{"ride request start (MYR-270)", "/api/ride-requests/crr0123456789abcdef0123456789abcd/start"},
		{"ride request dropped-off (MYR-270)", "/api/ride-requests/crr0123456789abcdef0123456789abcd/dropped-off"},
		{"ride request accept (MYR-175)", "/api/ride-requests/crr0123456789abcdef0123456789abcd/accept"},
		{"ride request decline (MYR-175)", "/api/ride-requests/crr0123456789abcdef0123456789abcd/decline"},
		{"vehicle command (MYR-180)", "/api/vehicles/clxyz1234567890abcdef/command/door_lock"},
		{"vehicle re-add (MYR-262)", "/api/tesla/vehicles/vid-12345/re-add"},
		{"vehicle refresh (MYR-315, §7.15)", "/api/tesla/vehicles/clxyz1234567890abcdef/refresh"},
		// MYR-184 vehicle sharing (§7.5). `/redeem` and `/{inviteId}/resend`
		// are both POST under /api/invites/, so mounting them is also the
		// assertion that ServeMux resolves the literal-vs-wildcard pair —
		// a collision would show up here as one of the two 404ing.
		{"share invite create (MYR-184, §7.5)", "/api/vehicles/clxyz1234567890abcdef/invites"},
		{"share invite resend (MYR-184, §7.5)", "/api/invites/csh0123456789abcdef0123456789abcd/resend"},
		{"share invite redeem (MYR-184, §7.5)", "/api/invites/redeem"},
	}
	for _, rt := range postRoutes {
		t.Run(rt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, rt.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code == http.StatusNotFound {
				t.Fatalf("route %q returned 404 — handler not mounted. Body: %s", rt.path, rec.Body.String())
			}
		})
	}

	// MYR-286 PUT route: the owner license-plate write (§7.14). Same rule —
	// an unauthenticated PUT must fail the bearer gate (401), never 404.
	putRoutes := []struct {
		name string
		path string
	}{
		{"vehicle license plate (MYR-286)", "/api/tesla/vehicles/clxyz1234567890abcdef/plate"},
		{"vehicle service window (MYR-316, §7.16)", "/api/tesla/vehicles/clxyz1234567890abcdef/service-window"},
		{"push device register (MYR-186, §7.17)", "/api/push/devices"},
	}
	for _, rt := range putRoutes {
		t.Run(rt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, rt.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code == http.StatusNotFound {
				t.Fatalf("route %q returned 404 — handler not mounted. Body: %s", rt.path, rec.Body.String())
			}
		})
	}

	// MYR-186 DELETE route: push device unregister on sign-out (§7.17). The
	// PUT and DELETE share a path, so mounting only one of the two verbs is a
	// live failure mode this catches — the other 404s.
	deleteRoutes := []struct {
		name string
		path string
	}{
		{"push device unregister (MYR-186, §7.17)", "/api/push/devices"},
		{"share invite revoke (MYR-184, §7.5)", "/api/invites/csh0123456789abcdef0123456789abcd"},
	}
	for _, rt := range deleteRoutes {
		t.Run(rt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, rt.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code == http.StatusNotFound {
				t.Fatalf("route %q returned 404 — handler not mounted. Body: %s", rt.path, rec.Body.String())
			}
		})
	}
}
