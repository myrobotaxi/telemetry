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
	"strings"
	"time"
)

const (
	proxyRequestTimeout = 30 * time.Second
	maxProxyResponse    = 1 << 16 // 64 KiB
)

// ProxyTransport is the concrete Transport: it forwards vehicle commands to
// the tesla-http-proxy sidecar (the same TESLA_PROXY_URL the fleet-config
// push already uses). The proxy signs signer-required commands with the
// virtual key and forwards unsigned commands (navigation_request) straight
// to the Fleet API. It also owns per-vehicle session caching, so there is
// no session state to manage in this process.
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

// post issues the HTTP request and returns (status, body, transport-error).
// The request target host is always the fixed, operator-configured proxy
// base — never attacker-controlled — and only validated, PathEscape'd path
// segments are interpolated.
func (t *ProxyTransport) post(ctx context.Context, endpoint, token string, body []byte) (status int, respBody []byte, err error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	// #nosec G107 -- endpoint host is the fixed proxy base from config, not
	// user input; path segments are PathEscape'd validated values.
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, reader) //nolint:gosec // host is fixed config
	if err != nil {
		return 0, nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}

	resp, err := t.client.Do(httpReq) //nolint:gosec // host is fixed config
	if err != nil {
		return 0, nil, fmt.Errorf("proxy request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err = io.ReadAll(io.LimitReader(resp.Body, maxProxyResponse))
	if err != nil {
		return 0, nil, fmt.Errorf("read proxy response: %w", err)
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
	// error paths, which carry NO envelope reason). These are opaque codes /
	// prose — no coordinates or tokens — but truncate defensively.
	reason := env.Response.Reason
	if reason == "" {
		reason = env.Error
	}
	reason = truncateReason(reason)

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
	case status == http.StatusBadRequest,
		containsAny(text, "invalid_command", "invalid parameter", "invalid request"):
		return TransportResult{Outcome: OutcomeInvalidRequest, Reason: reason}
	default:
		return TransportResult{Outcome: OutcomeFailed, Reason: reason}
	}
}

// maxReasonLen bounds the opaque detail string carried into logs/errors so a
// pathological upstream body cannot bloat a log line.
const maxReasonLen = 120

// truncateReason clamps a reason string to maxReasonLen runes (rune-safe so a
// multibyte boundary is never split).
func truncateReason(s string) string {
	r := []rune(s)
	if len(r) <= maxReasonLen {
		return s
	}
	return string(r[:maxReasonLen])
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
