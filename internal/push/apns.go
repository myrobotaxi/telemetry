package push

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
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

// apnsMessage is one addressed APNs delivery: the token to send to, the
// headers that describe the push, and the rendered JSON body.
//
// It exists because MYR-172 gave the client a SECOND kind of push. An alert and
// a Live Activity update differ in three headers (`apns-push-type`,
// `apns-topic` and `apns-priority`) and in nothing else — same host selection,
// same provider JWT, same retry policy, same status classification. Threading a
// value through the transport rather than branching inside it keeps that shared
// half honest: there is exactly one place that talks to Apple.
type apnsMessage struct {
	// deviceToken addresses the delivery — a device token for an alert, an
	// ActivityKit update token for a Live Activity. P1 either way.
	deviceToken string
	sandbox     bool
	pushType    string
	topic       string
	priority    string
	// expiration is the apns-expiration header, omitted when empty. APNs stores
	// and retries an undeliverable push until this instant.
	expiration string
	body       []byte
}

// Send delivers one notification, retrying once on a network error or 5xx.
// It maps APNs rejections onto ErrUnregistered / ErrThrottled for the caller.
func (c *Client) Send(ctx context.Context, n Notification) error {
	body, err := buildPayload(n)
	if err != nil {
		return err
	}

	return c.deliver(ctx, apnsMessage{
		deviceToken: n.DeviceToken,
		sandbox:     n.Sandbox,
		pushType:    pushTypeAlert,
		topic:       c.topic,
		priority:    priorityImmediate,
		body:        body,
	})
}

// deliver runs the retry loop for any message shape.
func (c *Client) deliver(ctx context.Context, m apnsMessage) error {
	const attempts = 2 // one try plus one retry
	var lastErr error
	for attempt := range attempts {
		var retryable bool
		retryable, lastErr = c.attempt(ctx, m)
		if !retryable {
			return lastErr
		}
		c.logger.Warn("apns send failed, retrying",
			slog.String("device_token_prefix", tokenPrefix(m.deviceToken)),
			slog.String("push_type", m.pushType),
			slog.Int("attempt", attempt+1),
			slog.String("error", lastErr.Error()),
		)
	}
	return lastErr
}

// attempt performs one HTTP round-trip. It reports whether the failure is worth
// one retry (network error or 5xx).
func (c *Client) attempt(ctx context.Context, m apnsMessage) (retryable bool, err error) {
	req, err := c.newRequest(ctx, m)
	if err != nil {
		return false, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// http.Client.Do ALWAYS returns *url.Error, whose Error() renders the
		// full request URL — which on this client ends in the P1 device or
		// activity token. This is the routine failure (a dropped connection, a
		// timeout) and deliver() logs it on every retry, so an unstripped wrap
		// here would put tokens in the log on an ordinary bad network day.
		// Unwrap to the transport cause and identify the token by prefix.
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return true, fmt.Errorf("push: apns request (token %s): %w", tokenPrefix(m.deviceToken), err)
	}
	defer func() { _ = resp.Body.Close() }()

	return classify(resp.StatusCode, drainReason(resp.Body))
}

// maxAPNsErrorBody caps the rejection body we read. Apple's is a two-field
// JSON object; the bound stops a misbehaving proxy from making us read a
// stream on the notification path.
const maxAPNsErrorBody = 4 << 10

// drainReason consumes the response body and returns APNs's `reason` string
// when there is one (`BadDeviceToken`, `ExpiredProviderToken`, `TopicDisallowed`,
// …). Draining matters twice over: it lets the connection be reused, and
// without the reason a 400 or 403 log line says only "status 400" — true, and
// useless, since the four things that produce a 403 need four different fixes.
// A body we cannot read or parse is not an error; the status alone still
// classifies the response.
func drainReason(body io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(body, maxAPNsErrorBody))
	// Drain whatever is left so the connection is reusable even if the body
	// exceeded the cap.
	_, _ = io.Copy(io.Discard, body)
	if err != nil || len(data) == 0 {
		return ""
	}

	var payload struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return ""
	}
	return payload.Reason
}

// classify maps an APNs status code (and its optional reason) onto
// (retryable, error).
func classify(status int, reason string) (bool, error) {
	switch {
	case status == http.StatusOK:
		return false, nil
	case status == http.StatusGone, status == http.StatusBadRequest:
		// 410 Unregistered, and 400 (BadDeviceToken / DeviceTokenNotForTopic).
		// Both mean this token will never accept another push.
		return false, withReason(ErrUnregistered, reason)
	case status == http.StatusTooManyRequests:
		return false, withReason(ErrThrottled, reason)
	case status >= http.StatusInternalServerError:
		return true, statusError(status, reason)
	default:
		return false, statusError(status, reason)
	}
}

// withReason annotates a sentinel with APNs's reason while keeping errors.Is
// working against the sentinel.
func withReason(sentinel error, reason string) error {
	if reason == "" {
		return sentinel
	}
	return fmt.Errorf("push: apns reason %s: %w", reason, sentinel)
}

// statusError renders a non-sentinel APNs rejection.
func statusError(status int, reason string) error {
	if reason == "" {
		return fmt.Errorf("push: apns status %d", status)
	}
	return fmt.Errorf("push: apns status %d (%s)", status, reason)
}

// APNs header values. The push type and the topic move together: a Live
// Activity update is rejected with TopicDisallowed unless BOTH the
// `.push-type.liveactivity` topic suffix and the matching apns-push-type are
// present, which is why buildActivityMessage sets them as a pair.
const (
	pushTypeAlert           = "alert"
	pushTypeLiveActivity    = "liveactivity"
	liveActivityTopicSuffix = ".push-type.liveactivity"

	// priorityImmediate (10) delivers now. Alerts and ride-lifecycle Activity
	// updates use it: they are the user-visible events.
	priorityImmediate = "10"
	// priorityConserving (5) lets APNs coalesce and defer. The ETA ticker uses
	// it, per MYR-194's "lifecycle events take priority over ETA ticks" — it is
	// also the header Apple's throttling budget is kindest to, which matters
	// for a push that fires every 60–90s for the length of a ride.
	priorityConserving = "5"
)

// newRequest builds the POST /3/device/{token} request with the APNs headers.
//
// THE TOKEN IS P1 AND THIS FUNCTION IS THE ONE PLACE IT IS INTERPOLATED INTO A
// STRING (data-classification.md §1.18, §3.2 — "never logged in full"). Two
// things keep that claim structurally true rather than merely intended:
//
//   - url.PathEscape. Any byte that could make the URL unparseable is
//     percent-encoded, so there is no input for which url.Parse fails and hands
//     back a *url.Error carrying the whole address — token included.
//   - the unwrap below. Even so, the error from a failed parse is stripped of
//     its *url.Error skin before it leaves, because that type's Error() prints
//     the URL verbatim and a `%w` of it would put the token in a log line the
//     moment anything upstream formatted the chain.
//
// The two are belt and braces on purpose: the first makes the failure
// unreachable, the second makes it harmless if a future net/url ever finds a
// new way to fail.
func (c *Client) newRequest(ctx context.Context, m apnsMessage) (*http.Request, error) {
	endpoint := c.host(m.sandbox) + "/3/device/" + url.PathEscape(m.deviceToken)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(m.body))
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		// Identify the token by its 8-character prefix, never by its value.
		return nil, fmt.Errorf("push: build apns request (token %s): %w", tokenPrefix(m.deviceToken), err)
	}

	providerToken, err := c.tokens.token()
	if err != nil {
		return nil, err
	}

	req.Header.Set("authorization", "bearer "+providerToken)
	req.Header.Set("apns-topic", m.topic)
	req.Header.Set("apns-push-type", m.pushType)
	req.Header.Set("apns-priority", m.priority)
	if m.expiration != "" {
		req.Header.Set("apns-expiration", m.expiration)
	}
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
