package commands

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// fleetRequestTimeout bounds a direct Fleet REST call. Matches the proxy
// timeout for consistency.
const fleetRequestTimeout = 30 * time.Second

// FleetRESTTransport sends UNSIGNED vehicle commands straight to the Tesla
// Fleet REST API, bypassing the tesla-http-proxy. It exists because proxy
// v0.4.1 mis-forwards REST-API commands (navigation_request): it returns a
// double-written 400 body ("command requires using the REST API" immediately
// followed by "upstream internal error"), so dispatch records invalid_request
// and the car is never reached. Posting the same body directly to the Fleet
// API returns 200 {"response":{"result":true,"queued":true}} and the car takes
// the destination (MYR-245, verified live 2026-07-19).
//
// The response envelope is the SAME {"response":{...},"error":...} shape as the
// proxy, so classifyResponse is reused verbatim: 200 result:true → OK, 408 →
// asleep (wake+retry), 401/403 → permission_denied, 400/422 → invalid_request.
//
// TLS is ALWAYS verified here (the client's default transport, or a caller-
// supplied verified client) — unlike the proxy's loopback InsecureSkipVerify
// client — because this dials the real, public Fleet API host.
type FleetRESTTransport struct {
	baseURL string
	client  *http.Client
	logger  *slog.Logger
}

// NewFleetRESTTransport builds a transport pointed at the Fleet API base URL
// (e.g. https://fleet-api.prd.na.vn.cloud.tesla.com). When baseURL is empty the
// transport reports Enabled()==false. When client is nil a default client with
// a 30s timeout and verified TLS is used.
func NewFleetRESTTransport(baseURL string, client *http.Client, logger *slog.Logger) *FleetRESTTransport {
	if client == nil {
		client = &http.Client{Timeout: fleetRequestTimeout}
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	return &FleetRESTTransport{baseURL: strings.TrimRight(baseURL, "/"), client: client, logger: logger}
}

// Enabled reports whether a Fleet API base URL is configured.
func (t *FleetRESTTransport) Enabled() bool { return t.baseURL != "" }

// Command POSTs to {base}/api/1/vehicles/{vin}/command/{command} with the
// owner's bearer token and classifies the Fleet API response.
func (t *FleetRESTTransport) Command(ctx context.Context, req TransportRequest) (TransportResult, error) {
	endpoint := fmt.Sprintf("%s/api/1/vehicles/%s/command/%s",
		t.baseURL, url.PathEscape(req.VIN), url.PathEscape(req.Command))

	status, respBody, err := httpPostCommand(ctx, t.client, endpoint, req.Token, req.Body)
	if err != nil {
		return TransportResult{}, err
	}
	return classifyResponse(status, respBody), nil
}

// Wake POSTs to {base}/api/1/vehicles/{vin}/wake_up. A non-2xx wake is an
// error but is non-fatal to the Executor's retry loop.
func (t *FleetRESTTransport) Wake(ctx context.Context, vin, token string) error {
	endpoint := fmt.Sprintf("%s/api/1/vehicles/%s/wake_up", t.baseURL, url.PathEscape(vin))
	status, _, err := httpPostCommand(ctx, t.client, endpoint, token, nil)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("wake_up returned status %d", status)
	}
	return nil
}
