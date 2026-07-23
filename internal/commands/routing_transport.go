package commands

import (
	"context"
	"log/slog"
)

// RoutingTransport is the Transport the Executor uses in production. It routes
// each command by TransportRequest.SignerRequired (MYR-245):
//
//   - SIGNED commands (door_lock, charge_start, …) → the tesla-http-proxy,
//     which signs them with the virtual key. Unchanged from before.
//   - UNSIGNED commands (navigation_request) → the Fleet REST API directly,
//     because proxy v0.4.1 mis-forwards REST-API commands.
//
// Enabled() reports the SIGNING (proxy) capability so the Executor still
// degrades signed commands to key_not_paired when no proxy is configured, and
// the wiring logs still read as "signing transport". RESTEnabled() reports the
// Fleet REST capability, which the config layer validates fail-fast — so
// unsigned commands never silently fall back to the proxy.
type RoutingTransport struct {
	proxy  *ProxyTransport
	fleet  *FleetRESTTransport
	logger *slog.Logger
}

var _ Transport = (*RoutingTransport)(nil)

// NewRoutingTransport composes the signing proxy and the Fleet REST transport.
// Either concrete transport may be disabled (empty base); the executor gate
// and per-request routing handle the degraded cases.
func NewRoutingTransport(proxy *ProxyTransport, fleet *FleetRESTTransport, logger *slog.Logger) *RoutingTransport {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	return &RoutingTransport{proxy: proxy, fleet: fleet, logger: logger}
}

// Enabled reports whether the signing (proxy) transport is configured.
func (t *RoutingTransport) Enabled() bool { return t.proxy.Enabled() }

// RESTEnabled reports whether the direct Fleet REST transport is configured.
func (t *RoutingTransport) RESTEnabled() bool { return t.fleet.Enabled() }

// Command routes to the proxy (signed) or the Fleet REST API (unsigned). A
// signed command with no proxy configured resolves to OutcomeNotPaired,
// mirroring the Executor's degraded-signing gate, so an unexpectedly-unsigned
// route never reaches the wrong endpoint.
func (t *RoutingTransport) Command(ctx context.Context, req TransportRequest) (TransportResult, error) {
	if req.SignerRequired {
		if !t.proxy.Enabled() {
			return TransportResult{Outcome: OutcomeNotPaired, Reason: "command signing transport not configured"}, nil
		}
		return t.proxy.Command(ctx, req)
	}
	return t.fleet.Command(ctx, req)
}

// Wake asks the vehicle to wake. It prefers the proxy when configured (the
// established, unchanged signed-command path); otherwise it wakes via Fleet
// REST. wake_up is a benign unsigned endpoint on both, and Wake is best-effort
// in the Executor's retry loop, so the choice is not load-bearing.
func (t *RoutingTransport) Wake(ctx context.Context, vin, token string) error {
	if t.proxy.Enabled() {
		return t.proxy.Wake(ctx, vin, token)
	}
	return t.fleet.Wake(ctx, vin, token)
}
