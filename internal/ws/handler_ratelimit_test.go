package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/tnando/my-robo-taxi-telemetry/internal/wserrors"
)

// TestHub_PerIPCapEmitsRateLimitedEnvelope is the MYR-47 reachability
// scenario for ErrCodeRateLimited. The pre-auth per-IP cap path is the
// only emitter in v1: once the cap is breached, the upgrade endpoint
// returns HTTP 429 with the contract-shaped REST envelope so SDK
// consumers branch on a single typed code regardless of the carrier
// transport (websocket-protocol.md §1.3 / §6.1.1, FR-7.1).
func TestHub_PerIPCapEmitsRateLimitedEnvelope(t *testing.T) {
	hub := newTestHub(t)
	t.Cleanup(hub.Stop)

	// Cap of 1 — first dial holds the slot; second is rejected with
	// 429 + envelope.
	auth := &testAuth{userID: "user-1", vehicleIDs: []string{"v-1"}}
	handler := hub.Handler(auth, HandlerConfig{
		MaxConnectionsPerIP: 1,
		AuthTimeout:         200 * time.Millisecond,
		WriteTimeout:        500 * time.Millisecond,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// First connection: open and authenticate. We force a synthetic
	// X-Forwarded-For so resolveClientIP keys on a stable IP rather
	// than r.RemoteAddr (which includes the ephemeral port — distinct
	// across dials and so unusable for cap testing without a header).
	const fakeIP = "10.42.0.1"
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer dialCancel()
	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/api/ws"
	first, dialResp, err := websocket.Dial(dialCtx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"X-Forwarded-For": []string{fakeIP}},
	})
	if err != nil {
		t.Fatalf("dial first: %v", err)
	}
	if dialResp != nil && dialResp.Body != nil {
		_ = dialResp.Body.Close()
	}
	t.Cleanup(func() { _ = first.Close(websocket.StatusNormalClosure, "") })

	authMsg := mustMarshalTest(t, wsMessage{
		Type:    msgTypeAuth,
		Payload: mustMarshalRaw(t, authPayload{Token: "token"}),
	})
	if err := first.Write(dialCtx, websocket.MessageText, authMsg); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	if _, _, err := first.Read(dialCtx); err != nil {
		t.Fatalf("read auth_ok: %v", err)
	}
	waitForClients(t, hub, 1)

	// Second connection from the same X-Forwarded-For: must be
	// rejected with 429 + envelope.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	httpURL := srv.URL + "/api/ws"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpURL, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("X-Forwarded-For", fakeIP)
	// Synthetic Upgrade headers — the per-IP cap check runs BEFORE the
	// websocket library would notice missing/incomplete handshake fields,
	// so a partial handshake is enough to exercise the rejection path.
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status: got %d, want %d", resp.StatusCode, http.StatusTooManyRequests)
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("content-type: got %q, want application/json; charset=utf-8", got)
	}

	var env wserrors.ErrorEnvelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Code != wserrors.ErrCodeRateLimited {
		t.Fatalf("code: got %q, want %q", env.Error.Code, wserrors.ErrCodeRateLimited)
	}
	if env.Error.Message == "" {
		t.Fatal("message must be non-empty")
	}
	if env.Error.SubCode != nil {
		t.Fatalf("REST v1 must emit subCode: null on per-IP rejection, got %q", *env.Error.SubCode)
	}
}
