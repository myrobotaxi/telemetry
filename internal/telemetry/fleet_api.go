package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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

	return &FleetAPIClient{
		baseURL:    baseURL,
		httpClient: &http.Client{Timeout: timeout},
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
	if len(req.VINs) == 0 {
		return nil, fmt.Errorf("PushTelemetryConfig: no VINs provided")
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("PushTelemetryConfig: marshal request: %w", err)
	}

	// The Fleet API uses a single endpoint for all VINs in the batch.
	// The first VIN in the path is required but the actual targets come
	// from the request body's "vins" array.
	url := c.baseURL + "/api/1/vehicles/" + req.VINs[0] + "/fleet_telemetry_config"

	c.logger.Debug("pushing telemetry config",
		slog.String("url", url),
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

// GetTelemetryErrors retrieves recent telemetry connection errors for a
// vehicle from the Fleet API. Useful for diagnosing why a vehicle is
// not sending telemetry.
func (c *FleetAPIClient) GetTelemetryErrors(
	ctx context.Context,
	token string,
	vin string,
) (*FleetErrorsResponse, error) {
	if vin == "" {
		return nil, fmt.Errorf("GetTelemetryErrors: empty VIN")
	}

	url := c.baseURL + "/api/1/vehicles/" + vin + "/fleet_telemetry_errors"

	c.logger.Debug("fetching telemetry errors",
		slog.String("vin", redactVIN(vin)),
	)

	respBody, err := c.doWithRetry(ctx, http.MethodGet, url, token, nil)
	if err != nil {
		return nil, fmt.Errorf("GetTelemetryErrors(%s): %w", redactVIN(vin), err)
	}

	var result FleetErrorsResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("GetTelemetryErrors(%s): decode response: %w", redactVIN(vin), err)
	}

	c.logger.Debug("telemetry errors retrieved",
		slog.String("vin", redactVIN(vin)),
		slog.Int("count", len(result.Response.Errors)),
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
				slog.String("url", url),
			)
		}

		req, err := http.NewRequestWithContext(ctx, method, url, newBodyReader(body))
		if err != nil {
			return nil, fmt.Errorf("create request: %w", err)
		}

		req.Header.Set("Authorization", "Bearer "+token)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("http request: %w", err)
			// Network errors are not retried — they are likely transient
			// DNS/TLS issues that won't resolve within retry window.
			return nil, lastErr
		}

		respBody, readErr := readLimitedBody(resp)
		resp.Body.Close()

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
