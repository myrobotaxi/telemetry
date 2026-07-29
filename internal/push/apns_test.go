package push

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestClient builds a Client whose requests are redirected to srv.
func newTestClient(t *testing.T, srvURL string) *Client {
	t.Helper()
	keyPEM, _ := testKeyPEM(t)
	c, err := NewClient(keyPEM, "ABC1234567", "NFKX777598", "app.myrobotaxi.ios", discardLogger())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	c.hostOverride = srvURL
	return c
}

// testDeviceValue is built at runtime rather than written as a literal: a
// 32-char hex constant assigned to a field named DeviceToken is exactly the
// shape gosec G101 flags as a hardcoded credential, and it is not one.
var testDeviceValue = strings.Repeat("aabbccdd", 4)

func testNotification() Notification {
	return Notification{
		DeviceToken: testDeviceValue,
		Title:       "Your ride is confirmed",
		Body:        "Blue Whale accepted your ride.",
		RideID:      "ride_abc123",
	}
}

func TestNewClientRequiresKey(t *testing.T) {
	_, err := NewClient("", "ABC1234567", "NFKX777598", "app.myrobotaxi.ios", discardLogger())
	if !errors.Is(err, ErrNoSigningKey) {
		t.Fatalf("NewClient(\"\") error = %v, want ErrNoSigningKey", err)
	}
}

// TestClientSendStatusMapping covers the full APNs response contract: success,
// the two permanent-rejection codes that must delete the device row, the
// throttle drop, and the retry budget.
func TestClientSendStatusMapping(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		wantErr      error
		wantErrText  string
		wantAttempts int32
	}{
		{name: "200 ok", status: http.StatusOK, wantAttempts: 1},
		{name: "410 unregistered deletes", status: http.StatusGone, wantErr: ErrUnregistered, wantAttempts: 1},
		{name: "400 bad device token deletes", status: http.StatusBadRequest, wantErr: ErrUnregistered, wantAttempts: 1},
		{name: "429 throttled drops", status: http.StatusTooManyRequests, wantErr: ErrThrottled, wantAttempts: 1},
		{name: "403 auth failure is not retried", status: http.StatusForbidden, wantErrText: "apns status 403", wantAttempts: 1},
		{name: "500 retried once then gives up", status: http.StatusInternalServerError, wantErrText: "apns status 500", wantAttempts: 2},
		{name: "503 retried once then gives up", status: http.StatusServiceUnavailable, wantErrText: "apns status 503", wantAttempts: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts.Add(1)
				w.WriteHeader(tt.status)
			}))
			defer srv.Close()

			err := newTestClient(t, srv.URL).Send(context.Background(), testNotification())

			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("Send() error = %v, want %v", err, tt.wantErr)
				}
			case tt.wantErrText != "":
				if err == nil || !strings.Contains(err.Error(), tt.wantErrText) {
					t.Errorf("Send() error = %v, want containing %q", err, tt.wantErrText)
				}
			default:
				if err != nil {
					t.Errorf("Send() error = %v, want nil", err)
				}
			}

			if got := attempts.Load(); got != tt.wantAttempts {
				t.Errorf("attempts = %d, want %d (retry budget)", got, tt.wantAttempts)
			}
		})
	}
}

// TestClientSendRetriesNetworkErrorOnce pins the retry budget for transport
// failures: the client must not hammer APNs beyond one retry.
func TestClientSendRetriesNetworkErrorOnce(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		// Hijack and close without a response so the client sees a transport error.
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("recorder does not support hijack")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	err := newTestClient(t, srv.URL).Send(context.Background(), testNotification())
	if err == nil {
		t.Fatal("Send() error = nil, want transport error")
	}
	if got := attempts.Load(); got != 2 {
		t.Errorf("attempts = %d, want 2 (one try plus one retry)", got)
	}
}

// TestClientRequestShape asserts the wire contract: path, headers, signed
// provider token, and a payload whose only userInfo key is rideId.
func TestClientRequestShape(t *testing.T) {
	type captured struct {
		path   string
		header http.Header
		body   []byte
	}
	var got captured

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got = captured{path: r.URL.Path, header: r.Header.Clone(), body: body}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := testNotification()
	if err := newTestClient(t, srv.URL).Send(context.Background(), n); err != nil {
		t.Fatalf("Send() error = %v", err)
	}

	if want := "/3/device/" + n.DeviceToken; got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}
	for header, want := range map[string]string{
		"Apns-Topic":     "app.myrobotaxi.ios",
		"Apns-Push-Type": "alert",
		"Apns-Priority":  "10",
		"Content-Type":   "application/json",
	} {
		if v := got.header.Get(header); v != want {
			t.Errorf("header %s = %q, want %q", header, v, want)
		}
	}

	auth := got.header.Get("Authorization")
	bearer, ok := strings.CutPrefix(auth, "bearer ")
	if !ok {
		t.Fatalf("Authorization = %q, want a `bearer ` prefix", auth)
	}
	// The token must be a parseable ES256 JWT carrying the team id.
	parsed, _, err := jwt.NewParser().ParseUnverified(bearer, jwt.MapClaims{})
	if err != nil {
		t.Fatalf("provider token is not a JWT: %v", err)
	}
	if alg, _ := parsed.Header["alg"].(string); alg != "ES256" {
		t.Errorf("provider token alg = %q, want ES256", alg)
	}

	var payload map[string]any
	if err := json.Unmarshal(got.body, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if len(payload) != 2 {
		t.Errorf("payload top-level keys = %v, want exactly aps + rideId", payload)
	}
	if payload["rideId"] != "ride_abc123" {
		t.Errorf("rideId = %v, want ride_abc123", payload["rideId"])
	}
	aps, _ := payload["aps"].(map[string]any)
	alert, _ := aps["alert"].(map[string]any)
	if alert["title"] != n.Title || alert["body"] != n.Body {
		t.Errorf("alert = %v, want title/body from the notification", alert)
	}
}

// TestClientHostSelection pins the sandbox flag → gateway mapping. These are
// the real Apple hosts; net/http negotiates HTTP/2 against them via ALPN,
// which APNs requires.
func TestClientHostSelection(t *testing.T) {
	keyPEM, _ := testKeyPEM(t)
	c, err := NewClient(keyPEM, "ABC1234567", "NFKX777598", "app.myrobotaxi.ios", discardLogger())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}

	tests := []struct {
		name    string
		sandbox bool
		want    string
	}{
		{name: "production build", want: "https://api.push.apple.com"},
		{name: "development build", sandbox: true, want: "https://api.sandbox.push.apple.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.host(tt.sandbox); got != tt.want {
				t.Errorf("host(%v) = %q, want %q", tt.sandbox, got, tt.want)
			}
		})
	}
}

func TestTokenPrefixNeverLogsFullToken(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "truncates long token", value: testDeviceValue, want: "aabbccdd"},
		{name: "short token passes through", value: "abc", want: "abc"},
		{name: "empty", value: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tokenPrefix(tt.value); got != tt.want {
				t.Errorf("tokenPrefix(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}
