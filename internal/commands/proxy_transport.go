package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	proxyRequestTimeout = 30 * time.Second
	maxProxyResponse    = 1 << 16 // 64 KiB
)

// ProxyTransport forwards SIGNED vehicle commands to the tesla-http-proxy
// sidecar (the same TESLA_PROXY_URL the fleet-config push already uses). The
// proxy signs signer-required commands with the virtual key and owns per-
// vehicle session caching, so there is no session state to manage in this
// process. Unsigned commands (navigation_request) do NOT go through here:
// proxy v0.4.1 mis-forwards REST-API commands (it double-writes a 400 body),
// so they route directly to the Fleet REST API via FleetRESTTransport — the
// RoutingTransport picks the endpoint per TransportRequest.SignerRequired
// (MYR-245).
type ProxyTransport struct {
	baseURL string
	client  *http.Client
	logger  *slog.Logger
}

// NewProxyTransport builds a transport pointed at the proxy base URL. When
// baseURL is empty the transport reports Enabled()==false and the Executor
// degrades to key_not_paired. When client is nil a default client with a
// 30s timeout is used (the caller passes proxyHTTPClient's loopback-aware
// client in production).
func NewProxyTransport(baseURL string, client *http.Client, logger *slog.Logger) *ProxyTransport {
	if client == nil {
		client = &http.Client{Timeout: proxyRequestTimeout}
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discard{}, nil))
	}
	return &ProxyTransport{baseURL: strings.TrimRight(baseURL, "/"), client: client, logger: logger}
}

// Enabled reports whether a proxy URL is configured.
func (t *ProxyTransport) Enabled() bool { return t.baseURL != "" }

// Command POSTs to {proxy}/api/1/vehicles/{vin}/command/{command}.
func (t *ProxyTransport) Command(ctx context.Context, req TransportRequest) (TransportResult, error) {
	endpoint := fmt.Sprintf("%s/api/1/vehicles/%s/command/%s",
		t.baseURL, url.PathEscape(req.VIN), url.PathEscape(req.Command))

	status, respBody, err := t.post(ctx, endpoint, req.Token, req.Body)
	if err != nil {
		return TransportResult{}, err
	}
	return classifyResponse(status, respBody), nil
}

// Wake POSTs to {proxy}/api/1/vehicles/{vin}/wake_up. A non-2xx wake is
// reported as an error but is non-fatal to the Executor's retry loop.
func (t *ProxyTransport) Wake(ctx context.Context, vin, token string) error {
	endpoint := fmt.Sprintf("%s/api/1/vehicles/%s/wake_up", t.baseURL, url.PathEscape(vin))
	status, _, err := t.post(ctx, endpoint, token, nil)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("wake_up returned status %d", status)
	}
	return nil
}

// post issues the HTTP request against the proxy and returns
// (status, body, transport-error). It delegates to httpPostCommand, the
// shared poster the Fleet REST transport also uses (MYR-245).
func (t *ProxyTransport) post(ctx context.Context, endpoint, token string, body []byte) (status int, respBody []byte, err error) {
	return httpPostCommand(ctx, t.client, endpoint, token, body)
}

// httpPostCommand POSTs a Bearer-authenticated JSON command body to endpoint
// and returns (status, body, transport-error). It is shared by ProxyTransport
// (loopback proxy base) and FleetRESTTransport (Fleet API base). The request
// target host is always a fixed, operator-configured base — never attacker-
// controlled — and only validated, PathEscape'd path segments are interpolated.
func httpPostCommand(ctx context.Context, client *http.Client, endpoint, token string, body []byte) (status int, respBody []byte, err error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	// #nosec G107 -- endpoint host is a fixed config base (proxy loopback or
	// Fleet API), not user input; path segments are PathEscape'd validated values.
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, reader) //nolint:gosec // host is fixed config
	if err != nil {
		return 0, nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(httpReq) //nolint:gosec // host is fixed config
	if err != nil {
		return 0, nil, fmt.Errorf("command request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err = io.ReadAll(io.LimitReader(resp.Body, maxProxyResponse))
	if err != nil {
		return 0, nil, fmt.Errorf("read command response: %w", err)
	}
	return resp.StatusCode, respBody, nil
}

// classifyResponse maps a proxy/Fleet HTTP response to a TransportResult.
// Tesla's command envelope is {"response":{"result":bool,"reason":string}}
// on 200; error paths carry an {"error":...} string. The mapping is
// keyword-based because the proxy relays Tesla's own (unstable) prose and
// there is no stable machine code for these conditions.
func classifyResponse(status int, body []byte) TransportResult {
	var env struct {
		Response struct {
			Result bool   `json:"result"`
			Reason string `json:"reason"`
		} `json:"response"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(body, &env)

	text := strings.ToLower(string(body))

	// 200 with an explicit positive result is the only success.
	if status >= 200 && status < 300 && env.Response.Result {
		return TransportResult{Outcome: OutcomeOK}
	}

	// reason is the opaque detail surfaced in logs + errors. Prefer Tesla's
	// command-envelope reason; fall back to the top-level {"error":...} string
	// (the shape the proxy uses for its own 400 `invalid_command` and other
	// error paths, which carry NO envelope reason). It is sanitized before it
	// can reach any log or CommandError.Detail (see sanitizeReason).
	reason := env.Response.Reason
	if reason == "" {
		reason = env.Error
	}
	reason = sanitizeReason(reason)

	switch {
	case containsAny(text, "not paired", "not been paired", "unpaired", "add your key",
		"missing key", "keys_not_configured", "no key", "could not find", "unregistered"):
		return TransportResult{Outcome: OutcomeNotPaired, Reason: reason}
	case status == http.StatusRequestTimeout,
		containsAny(text, "asleep", "unavailable", "not available", "offline", "waking", "vehicle is not awake"):
		return TransportResult{Outcome: OutcomeAsleep, Reason: reason}
	case containsAny(text, "counter", "anti-replay", "invalid signature", "session"):
		return TransportResult{Outcome: OutcomeCounterError, Reason: reason}
	case status == http.StatusForbidden, status == http.StatusUnauthorized,
		containsAny(text, "scope", "not authorized", "forbidden"):
		return TransportResult{Outcome: OutcomePermissionDenied, Reason: reason}
	case status == http.StatusBadRequest, status == http.StatusUnprocessableEntity,
		containsAny(text, "invalid_command", "invalid parameter", "invalid request"):
		// Fleet REST rejects malformed command params with 400 or 422 (MYR-245).
		return TransportResult{Outcome: OutcomeInvalidRequest, Reason: reason}
	default:
		return TransportResult{Outcome: OutcomeFailed, Reason: reason}
	}
}

// maxReasonLen bounds the opaque detail string carried into logs/errors so a
// pathological upstream body cannot bloat a log line.
const maxReasonLen = 120

var (
	// reasonURLRe matches any URL (scheme://…) up to the next whitespace, so a
	// maps link (and any coordinates embedded in it) is stripped whole.
	reasonURLRe = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://\S+`)
	// reasonCoordRe matches a signed-decimal coordinate pair (lat,lon), the
	// one P1 shape that could otherwise slip through the charset filter (which
	// keeps digits, '.', and '-').
	reasonCoordRe = regexp.MustCompile(`-?\d+\.\d+\s*,\s*-?\d+\.\d+`)
)

// sanitizeReason hardens the opaque upstream reason before it can reach any
// log or CommandError.Detail. It is defense-in-depth: these strings are
// expected to be opaque codes/prose (e.g. `invalid_command`), but the upstream
// is untrusted, so we ENFORCE — not merely assume — that no P1 value leaks:
//  1. strip any URL (a maps link would carry embedded coordinates),
//  2. strip any signed-decimal coordinate pair,
//  3. lowercase and collapse to the [a-z0-9_ .:-] charset (dropping anything
//     else, including stray digits left after coordinate removal are kept only
//     if they are not a pair — single tokens like an HTTP status are harmless),
//  4. trim and truncate to maxReasonLen runes.
func sanitizeReason(s string) string {
	s = strings.ToLower(s)
	s = reasonURLRe.ReplaceAllString(s, "")
	s = reasonCoordRe.ReplaceAllString(s, "")

	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case strings.ContainsRune("_ .:-", r):
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())

	rn := []rune(out)
	if len(rn) > maxReasonLen {
		out = string(rn[:maxReasonLen])
	}
	return out
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
