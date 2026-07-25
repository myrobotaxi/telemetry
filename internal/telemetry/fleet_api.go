package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	neturl "net/url"
	"time"
)

const (
	// defaultFleetAPITimeout is the HTTP timeout for Fleet API requests.
	defaultFleetAPITimeout = 30 * time.Second

	// defaultFleetAPIBaseURL is the North America Fleet API endpoint.
	defaultFleetAPIBaseURL = "https://fleet-api.prd.na.vn.cloud.tesla.com"

	// maxResponseBody limits how much of an error response body we read
	// to avoid unbounded memory use on malicious responses.
	maxResponseBody = 1 << 16 // 64 KiB
)

// FleetAPIConfig holds settings for the Fleet API client.
type FleetAPIConfig struct {
	BaseURL    string
	Timeout    time.Duration
	MaxRetries int
	// HTTPClient is an optional pre-configured HTTP client. If nil, a default
	// client is created with the configured Timeout. Use this to inject a
	// custom transport (e.g., for mTLS or proxy support).
	HTTPClient *http.Client
}

// FleetAPIClient communicates with Tesla's Fleet API to push telemetry
// configuration to vehicles and retrieve telemetry errors.
type FleetAPIClient struct {
	baseURL    string
	httpClient *http.Client
	logger     *slog.Logger
	retry      retryPolicy
}

// NewFleetAPIClient creates a FleetAPIClient with the given config and
// logger. If cfg.BaseURL is empty, the North America endpoint is used.
// If cfg.Timeout is zero, a 30-second timeout is applied.
func NewFleetAPIClient(cfg FleetAPIConfig, logger *slog.Logger) *FleetAPIClient {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultFleetAPIBaseURL
	}

	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = defaultFleetAPITimeout
	}

	rp := defaultRetryPolicy()
	if cfg.MaxRetries > 0 {
		rp.MaxRetries = cfg.MaxRetries
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}

	return &FleetAPIClient{
		baseURL:    baseURL,
		httpClient: httpClient,
		logger:     logger,
		retry:      rp,
	}
}

// PushTelemetryConfig sends a telemetry configuration to the Fleet API
// for the vehicles listed in req.VINs. The token must be a valid OAuth2
// Bearer token for the fleet owner.
func (c *FleetAPIClient) PushTelemetryConfig(
	ctx context.Context,
	token string,
	req FleetConfigRequest,
) (*FleetConfigResponse, error) {
	if token == "" {
		return nil, fmt.Errorf("PushTelemetryConfig: auth token is required")
	}
	if len(req.VINs) == 0 {
		return nil, fmt.Errorf("PushTelemetryConfig: no VINs provided")
	}
	for _, vin := range req.VINs {
		if len(vin) != 17 {
			return nil, fmt.Errorf("PushTelemetryConfig: invalid VIN %q (must be 17 characters)", redactVIN(vin))
		}
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("PushTelemetryConfig: marshal request: %w", err)
	}

	// The tesla-http-proxy expects /api/1/vehicles/fleet_telemetry_config
	// (no VIN in the URL). VINs are passed in the request body's "vins" array.
	// The proxy signs the config into a JWS token and forwards it to Tesla.
	url := c.baseURL + "/api/1/vehicles/fleet_telemetry_config"

	c.logger.Debug("pushing telemetry config",
		slog.String("vin", redactVIN(req.VINs[0])),
		slog.Int("vin_count", len(req.VINs)),
	)

	respBody, err := c.doWithRetry(ctx, http.MethodPost, url, token, body)
	if err != nil {
		return nil, fmt.Errorf("PushTelemetryConfig: %w", err)
	}

	var result FleetConfigResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("PushTelemetryConfig: decode response: %w", err)
	}

	c.logger.Info("telemetry config pushed",
		slog.Int("updated", result.Response.UpdatedVehicles),
		slog.Int("skipped", len(result.Response.SkippedVehicles)),
	)

	return &result, nil
}

// doWithRetry executes an HTTP request with retry logic for rate
// limiting (429) and server errors (5xx).
func (c *FleetAPIClient) doWithRetry(
	ctx context.Context,
	method string,
	url string,
	token string,
	body []byte,
) ([]byte, error) {
	var lastErr error

	for attempt := range c.retry.MaxRetries + 1 {
		if attempt > 0 {
			c.logger.Debug("retrying fleet API request",
				slog.Int("attempt", attempt),
				slog.String("method", method),
			)
		}

		// #nosec G107 G704 -- SSRF taint (reported as G704 by the pinned
		// gosec; G107 is the classic URL-taint id): url's scheme+host is
		// always the fixed Fleet API base (config, not user input); callers
		// only interpolate validated, PathEscape'd VINs as path segments, so
		// the request target host is never attacker-controlled.
		req, err := http.NewRequestWithContext(ctx, method, url, newBodyReader(body)) //nolint:gosec // host is fixed config — see #nosec note above
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}

		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		// #nosec G107 G704 -- request target host is the fixed Fleet API
		// base (see note above); no attacker control of scheme/host.
		resp, err := c.httpClient.Do(req) //nolint:gosec // host is fixed config
		if err != nil {
			// A transport error from Do is a *url.Error whose Error() embeds the
			// full request URL — which carries the VIN as a path segment. Strip
			// the URL and surface only the underlying cause so the VIN never
			// leaks into an error string (and thus logs). Network errors are not
			// retried — they are likely transient DNS/TLS issues.
			lastErr = fmt.Errorf("http %s request failed: %w", method, transportCause(err))
			return nil, lastErr
		}

		respBody, readErr := func() ([]byte, error) {
			defer resp.Body.Close()
			return readLimitedBody(resp)
		}()

		if readErr != nil {
			return nil, fmt.Errorf("read response: %w", readErr)
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return respBody, nil
		}

		lastErr = &FleetAPIError{
			StatusCode: resp.StatusCode,
			Body:       string(respBody),
		}

		if !isRetryable(resp.StatusCode) || attempt == c.retry.MaxRetries {
			return nil, lastErr
		}

		delay := retryDelay(resp, attempt, c.retry)
		c.logger.Warn("fleet API request failed, will retry",
			slog.Int("status", resp.StatusCode),
			slog.Duration("delay", delay),
			slog.Int("attempt", attempt+1),
			slog.Int("max_retries", c.retry.MaxRetries),
		)

		if err := sleepWithContext(ctx, delay); err != nil {
			return nil, err
		}
	}

	return nil, lastErr
}

// transportCause unwraps a *url.Error so its embedded request URL (which
// carries the VIN as a path segment) never reaches the returned error string.
// Non-url errors pass through unchanged.
func transportCause(err error) error {
	var uerr *neturl.Error
	if errors.As(err, &uerr) && uerr.Err != nil {
		return uerr.Err
	}
	return err
}

// newBodyReader returns a reader for the given body bytes, or nil if
// body is nil (for GET requests).
func newBodyReader(body []byte) io.Reader {
	if body == nil {
		return nil
	}
	return bytes.NewReader(body)
}

// readLimitedBody reads up to maxResponseBody bytes from the response.
func readLimitedBody(resp *http.Response) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	return data, nil
}
