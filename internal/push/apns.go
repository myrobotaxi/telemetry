package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// APNs production and development hosts. The device row's sandbox flag picks
// between them: a token minted by a development build is only valid against
// the sandbox gateway and vice versa.
const (
	hostProduction = "https://api.push.apple.com"
	hostSandbox    = "https://api.sandbox.push.apple.com"
)

// sendTimeout bounds a single HTTP attempt. Pushes are best-effort garnish on
// the ride flow, so the budget is deliberately small.
const sendTimeout = 10 * time.Second

// Client is the real APNs sender. It speaks HTTP/2 (required by APNs) over
// Go's net/http, which negotiates h2 via ALPN against these hosts — verified
// against api.sandbox.push.apple.com, which answers HTTP/2.0.
type Client struct {
	httpClient *http.Client
	tokens     *tokenSource
	topic      string
	logger     *slog.Logger

	// hostOverride, when non-empty, replaces both APNs hosts. Tests point it
	// at an httptest server; production leaves it empty.
	hostOverride string
}

// NewClient builds an APNs client from an APNs .p8 PEM. It returns
// ErrNoSigningKey when keyPEM is empty so callers can run keyless.
func NewClient(keyPEM, keyID, teamID, topic string, logger *slog.Logger) (*Client, error) {
	key, err := parseP8(keyPEM)
	if err != nil {
		return nil, err
	}
	return &Client{
		httpClient: &http.Client{
			// ForceAttemptHTTP2 keeps h2 negotiation on for this explicit
			// transport (net/http disables it only when a custom TLS config
			// is supplied, which we do not do).
			Transport: &http.Transport{ForceAttemptHTTP2: true},
			Timeout:   sendTimeout,
		},
		tokens: newTokenSource(key, keyID, teamID),
		topic:  topic,
		logger: logger,
	}, nil
}

// Send delivers one notification, retrying once on a network error or 5xx.
// It maps APNs rejections onto ErrUnregistered / ErrThrottled for the caller.
func (c *Client) Send(ctx context.Context, n Notification) error {
	body, err := buildPayload(n)
	if err != nil {
		return err
	}

	const attempts = 2 // one try plus one retry
	var lastErr error
	for attempt := range attempts {
		var retryable bool
		retryable, lastErr = c.attempt(ctx, n, body)
		if !retryable {
			return lastErr
		}
		c.logger.Warn("apns send failed, retrying",
			slog.String("device_token_prefix", tokenPrefix(n.DeviceToken)),
			slog.Int("attempt", attempt+1),
			slog.String("error", lastErr.Error()),
		)
	}
	return lastErr
}

// attempt performs one HTTP round-trip. It reports whether the failure is worth
// one retry (network error or 5xx).
func (c *Client) attempt(ctx context.Context, n Notification, body []byte) (retryable bool, err error) {
	req, err := c.newRequest(ctx, n, body)
	if err != nil {
		return false, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return true, fmt.Errorf("push: apns request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	return classify(resp.StatusCode)
}

// classify maps an APNs status code onto (retryable, error).
func classify(status int) (bool, error) {
	switch {
	case status == http.StatusOK:
		return false, nil
	case status == http.StatusGone, status == http.StatusBadRequest:
		// 410 Unregistered, and 400 (BadDeviceToken / DeviceTokenNotForTopic).
		// Both mean this token will never accept another push.
		return false, ErrUnregistered
	case status == http.StatusTooManyRequests:
		return false, ErrThrottled
	case status >= http.StatusInternalServerError:
		return true, fmt.Errorf("push: apns status %d", status)
	default:
		return false, fmt.Errorf("push: apns status %d", status)
	}
}

// newRequest builds the POST /3/device/{token} request with the APNs headers.
func (c *Client) newRequest(ctx context.Context, n Notification, body []byte) (*http.Request, error) {
	url := fmt.Sprintf("%s/3/device/%s", c.host(n.Sandbox), n.DeviceToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("push: build apns request: %w", err)
	}

	providerToken, err := c.tokens.token()
	if err != nil {
		return nil, err
	}

	req.Header.Set("authorization", "bearer "+providerToken)
	req.Header.Set("apns-topic", c.topic)
	req.Header.Set("apns-push-type", "alert")
	req.Header.Set("apns-priority", "10")
	req.Header.Set("content-type", "application/json")
	return req, nil
}

// host picks the APNs gateway for the device's build flavour.
func (c *Client) host(sandbox bool) string {
	if c.hostOverride != "" {
		return c.hostOverride
	}
	if sandbox {
		return hostSandbox
	}
	return hostProduction
}

// buildPayload renders the APNs JSON body. Everything outside the `aps` object
// reaches the app as userInfo, and by policy that is exactly one key: rideId.
func buildPayload(n Notification) ([]byte, error) {
	payload := map[string]any{
		"aps": map[string]any{
			"alert": map[string]any{
				"title": n.Title,
				"body":  n.Body,
			},
			"sound": "default",
		},
		"rideId": n.RideID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("push: marshal apns payload: %w", err)
	}
	return body, nil
}
