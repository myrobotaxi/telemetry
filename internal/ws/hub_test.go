package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// testAuth is an Authenticator for tests with configurable behavior.
type testAuth struct {
	userID     string
	vehicleIDs []string
	err        error
}

func (a *testAuth) ValidateToken(_ context.Context, token string) (string, error) {
	if a.err != nil {
		return "", a.err
	}
	if token == "" {
		return "", ErrInvalidToken
	}
	return a.userID, nil
}

func (a *testAuth) GetUserVehicles(_ context.Context, _ string) ([]string, error) {
	return a.vehicleIDs, nil
}

// waitForClients polls until the hub reaches the desired client count or
// times out. This replaces brittle time.Sleep calls in tests.
func waitForClients(t *testing.T, hub *Hub, want int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if hub.ClientCount() == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %d clients, got %d", want, hub.ClientCount())
		case <-tick.C:
		}
	}
}

// newTestServer creates an httptest.Server serving the Hub's WebSocket
// handler with the given auth.
func newTestServer(t *testing.T, hub *Hub, auth Authenticator) *httptest.Server {
	t.Helper()
	handler := hub.Handler(auth, HandlerConfig{
		AuthTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
	return httptest.NewServer(handler)
}

// dialAndAuth connects to the test server, sends an auth message, and
// consumes the auth_ok response frame. Tests that need to inspect auth_ok
// directly should use dialAndAuthRaw instead.
func dialAndAuth(t *testing.T, url, token string) *websocket.Conn {
	t.Helper()
	conn := dialAndAuthRaw(t, url, token)

	// Consume the auth_ok frame so downstream reads see only data frames.
	msg := readMessage(t, conn)
	if msg.Type != msgTypeAuthOk {
		t.Fatalf("dialAndAuth: expected first frame %q, got %q", msgTypeAuthOk, msg.Type)
	}
	return conn
}

// dialAndAuthRaw connects and sends auth but does NOT read the auth_ok
// response. Use this when you need to inspect the auth_ok frame directly.
func dialAndAuthRaw(t *testing.T, url, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := strings.Replace(url, "http://", "ws://", 1) + "/api/ws"
	conn, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}

	authMsg := mustMarshalTest(t, wsMessage{
		Type:    msgTypeAuth,
		Payload: mustMarshalRaw(t, authPayload{Token: token}),
	})
	if err := conn.Write(ctx, websocket.MessageText, authMsg); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	return conn
}

// dialOnly connects without sending auth.
func dialOnly(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	wsURL := strings.Replace(url, "http://", "ws://", 1) + "/api/ws"
	conn, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	return conn
}

// testReadTimeout is the default read timeout for test helpers.
const testReadTimeout = 2 * time.Second

// readMessage reads a single JSON message from the WebSocket.
func readMessage(t *testing.T, conn *websocket.Conn) wsMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testReadTimeout)
	defer cancel()

	_, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var msg wsMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return msg
}

func mustMarshalTest(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

func mustMarshalRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	return json.RawMessage(mustMarshalTest(t, v))
}

func newTestHub(t *testing.T) *Hub {
	t.Helper()
	return NewHub(slog.Default(), NoopHubMetrics{})
}

func TestHub_AuthFlow(t *testing.T) {
	hub := newTestHub(t)
	t.Cleanup(hub.Stop)

	auth := &testAuth{userID: "user-1", vehicleIDs: []string{"v-1", "v-2"}}
	srv := newTestServer(t, hub, auth)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "valid-token")
	defer conn.Close(websocket.StatusNormalClosure, "")

	waitForClients(t, hub, 1)

	if got := hub.ClientCount(); got != 1 {
		t.Fatalf("expected 1 client, got %d", got)
	}
}

func TestHub_AuthFailure_InvalidToken(t *testing.T) {
	hub := newTestHub(t)
	t.Cleanup(hub.Stop)

	auth := &testAuth{err: ErrInvalidToken}
	srv := newTestServer(t, hub, auth)
	t.Cleanup(srv.Close)

	conn := dialAndAuthRaw(t, srv.URL, "bad-token")
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Should receive an error message before disconnect.
	msg := readMessage(t, conn)
	if msg.Type != msgTypeError {
		t.Fatalf("expected error message, got %q", msg.Type)
	}

	var errPayload errorPayload
	if err := json.Unmarshal(msg.Payload, &errPayload); err != nil {
		t.Fatalf("unmarshal error payload: %v", err)
	}
	if errPayload.Code != errCodeAuthFailed {
		t.Fatalf("expected code %q, got %q", errCodeAuthFailed, errPayload.Code)
	}
}

func TestHub_AuthTimeout(t *testing.T) {
	hub := newTestHub(t)
	t.Cleanup(hub.Stop)

	auth := &testAuth{userID: "user-1", vehicleIDs: []string{"v-1"}}
	handler := hub.Handler(auth, HandlerConfig{
		AuthTimeout:  200 * time.Millisecond,
		WriteTimeout: 2 * time.Second,
	})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// Connect but do NOT send auth.
	conn := dialOnly(t, srv.URL)
	defer conn.Close(websocket.StatusNormalClosure, "")

	// The server should close the connection after the auth timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, _, err := conn.Read(ctx)
	if err == nil {
		t.Fatal("expected read error after auth timeout, got nil")
	}
}

func TestHub_Broadcast_AuthorizedOnly(t *testing.T) {
	hub := newTestHub(t)
	t.Cleanup(hub.Stop)

	// Client 1 sees vehicle v-1.
	auth1 := &testAuth{userID: "user-1", vehicleIDs: []string{"v-1"}}
	srv1 := newTestServer(t, hub, auth1)
	t.Cleanup(srv1.Close)
	conn1 := dialAndAuth(t, srv1.URL, "token-1")
	defer conn1.Close(websocket.StatusNormalClosure, "")

	// Client 2 sees vehicle v-2.
	auth2 := &testAuth{userID: "user-2", vehicleIDs: []string{"v-2"}}
	srv2 := newTestServer(t, hub, auth2)
	t.Cleanup(srv2.Close)
	conn2 := dialAndAuth(t, srv2.URL, "token-2")
	defer conn2.Close(websocket.StatusNormalClosure, "")

	waitForClients(t, hub, 2)

	// Broadcast for v-1 only.
	updateMsg := mustMarshalTest(t, wsMessage{
		Type: msgTypeVehicleUpdate,
		Payload: mustMarshalRaw(t, vehicleUpdatePayload{
			VehicleID: "v-1",
			Fields:    map[string]any{"speed": 65},
			Timestamp: time.Now().Format(time.RFC3339),
		}),
	})
	hub.Broadcast("v-1", updateMsg)

	// Client 1 should receive the update.
	msg := readMessage(t, conn1)
	if msg.Type != msgTypeVehicleUpdate {
		t.Fatalf("client1: expected %q, got %q", msgTypeVehicleUpdate, msg.Type)
	}

	// Client 2 should NOT receive anything. Try reading with a short timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, _, err := conn2.Read(ctx)
	if err == nil {
		t.Fatal("client2: expected no message for unauthorized vehicle, but got one")
	}
}

func TestHub_Heartbeat(t *testing.T) {
	hub := newTestHub(t)
	t.Cleanup(hub.Stop)

	auth := &testAuth{userID: "user-1", vehicleIDs: []string{"v-1"}}
	srv := newTestServer(t, hub, auth)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "token")
	defer conn.Close(websocket.StatusNormalClosure, "")

	waitForClients(t, hub, 1)

	// Start heartbeat with a short interval for testing.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hub.RunHeartbeat(ctx, 100*time.Millisecond)

	// Should receive at least one heartbeat.
	msg := readMessage(t, conn)
	if msg.Type != msgTypeHeartbeat {
		t.Fatalf("expected heartbeat, got %q", msg.Type)
	}
}

func TestHub_SlowClient_DropOldest(t *testing.T) {
	hub := newTestHub(t)
	t.Cleanup(hub.Stop)

	auth := &testAuth{userID: "slow-user", vehicleIDs: []string{"v-1"}}
	srv := newTestServer(t, hub, auth)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "token")
	defer conn.Close(websocket.StatusNormalClosure, "")

	waitForClients(t, hub, 1)

	// Fill the send buffer beyond capacity without reading.
	for i := range sendBufSize + 10 {
		msg := mustMarshalTest(t, wsMessage{
			Type: msgTypeVehicleUpdate,
			Payload: mustMarshalRaw(t, vehicleUpdatePayload{
				VehicleID: "v-1",
				Fields:    map[string]any{"seq": i},
				Timestamp: time.Now().Format(time.RFC3339),
			}),
		})
		hub.Broadcast("v-1", msg)
	}

	// Should still be able to read messages (oldest were dropped).
	msg := readMessage(t, conn)
	if msg.Type != msgTypeVehicleUpdate {
		t.Fatalf("expected vehicle_update after buffer overflow, got %q", msg.Type)
	}
}

func TestHub_MultipleClients_IndependentBuffers(t *testing.T) {
	hub := newTestHub(t)
	t.Cleanup(hub.Stop)

	auth := &testAuth{userID: "user-shared", vehicleIDs: []string{"v-shared"}}
	srv := newTestServer(t, hub, auth)
	t.Cleanup(srv.Close)

	const numClients = 3
	conns := make([]*websocket.Conn, numClients)
	for i := range numClients {
		conns[i] = dialAndAuth(t, srv.URL, "token")
		defer conns[i].Close(websocket.StatusNormalClosure, "")
	}

	waitForClients(t, hub, numClients)

	if got := hub.ClientCount(); got != numClients {
		t.Fatalf("expected %d clients, got %d", numClients, got)
	}

	// Broadcast one message.
	msg := mustMarshalTest(t, wsMessage{
		Type: msgTypeVehicleUpdate,
		Payload: mustMarshalRaw(t, vehicleUpdatePayload{
			VehicleID: "v-shared",
			Fields:    map[string]any{"speed": 42},
			Timestamp: time.Now().Format(time.RFC3339),
		}),
	})
	hub.Broadcast("v-shared", msg)

	// All clients should receive it.
	var wg sync.WaitGroup
	for i, conn := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := readMessage(t, conn)
			if got.Type != msgTypeVehicleUpdate {
				t.Errorf("client %d: expected %q, got %q", i, msgTypeVehicleUpdate, got.Type)
			}
		}()
	}
	wg.Wait()
}

func TestHub_Stop_ClosesAllClients(t *testing.T) {
	hub := newTestHub(t)

	auth := &testAuth{userID: "user-1", vehicleIDs: []string{"v-1"}}
	srv := newTestServer(t, hub, auth)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "token")
	defer conn.Close(websocket.StatusNormalClosure, "")

	waitForClients(t, hub, 1)

	if got := hub.ClientCount(); got != 1 {
		t.Fatalf("expected 1 client, got %d", got)
	}

	hub.Stop()

	if got := hub.ClientCount(); got != 0 {
		t.Fatalf("expected 0 clients after stop, got %d", got)
	}
}

func TestHub_Handler_NonWSRequest(t *testing.T) {
	hub := newTestHub(t)
	t.Cleanup(hub.Stop)

	auth := &testAuth{userID: "user-1"}
	handler := hub.Handler(auth, HandlerConfig{})

	// Non-WebSocket GET should fail gracefully.
	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/ws", nil)
	handler.ServeHTTP(rec, req)

	// The websocket library returns 426 or similar; we just check it's not 200.
	if rec.Code == http.StatusOK {
		t.Fatal("expected non-200 for plain HTTP request to WS endpoint")
	}
}

func TestClient_HasVehicle(t *testing.T) {
	tests := []struct {
		name       string
		vehicleIDs []string
		query      string
		want       bool
	}{
		{
			name:       "vehicle in list",
			vehicleIDs: []string{"v-1", "v-2", "v-3"},
			query:      "v-2",
			want:       true,
		},
		{
			name:       "vehicle not in list",
			vehicleIDs: []string{"v-1", "v-2"},
			query:      "v-3",
			want:       false,
		},
		{
			name:       "nil list grants all-vehicle access",
			vehicleIDs: nil,
			query:      "v-1",
			want:       true,
		},
		{
			name:       "empty slice grants all-vehicle access",
			vehicleIDs: []string{},
			query:      "v-1",
			want:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Client{vehicleIDs: tt.vehicleIDs}
			if got := c.hasVehicle(tt.query); got != tt.want {
				t.Fatalf("hasVehicle(%q) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestClient_Enqueue_DropOldest(t *testing.T) {
	c := &Client{send: make(chan []byte, 2)}

	// Fill buffer.
	if dropped := c.enqueue([]byte("msg-1")); dropped {
		t.Fatal("unexpected drop on first enqueue")
	}
	if dropped := c.enqueue([]byte("msg-2")); dropped {
		t.Fatal("unexpected drop on second enqueue")
	}

	// Third message should trigger drop-oldest.
	if dropped := c.enqueue([]byte("msg-3")); !dropped {
		t.Fatal("expected drop on third enqueue")
	}

	// Read messages — should get msg-2 and msg-3 (msg-1 was dropped).
	got1 := <-c.send
	got2 := <-c.send
	if string(got1) != "msg-2" {
		t.Fatalf("expected msg-2, got %q", string(got1))
	}
	if string(got2) != "msg-3" {
		t.Fatalf("expected msg-3, got %q", string(got2))
	}
}

func TestNoopAuthenticator(t *testing.T) {
	t.Run("empty token rejected", func(t *testing.T) {
		auth := &NoopAuthenticator{}
		_, err := auth.ValidateToken(context.Background(), "")
		if err == nil {
			t.Fatal("expected error for empty token")
		}
	})

	t.Run("any non-empty token accepted", func(t *testing.T) {
		auth := &NoopAuthenticator{UserID: "custom-user"}
		userID, err := auth.ValidateToken(context.Background(), "anything")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if userID != "custom-user" {
			t.Fatalf("expected custom-user, got %q", userID)
		}
	})

	t.Run("default user ID", func(t *testing.T) {
		auth := &NoopAuthenticator{}
		userID, err := auth.ValidateToken(context.Background(), "token")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if userID != "test-user" {
			t.Fatalf("expected test-user, got %q", userID)
		}
	})

	t.Run("vehicle IDs returned", func(t *testing.T) {
		auth := &NoopAuthenticator{VehicleIDs: []string{"v-1", "v-2"}}
		ids, err := auth.GetUserVehicles(context.Background(), "any")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(ids) != 2 {
			t.Fatalf("expected 2 vehicle IDs, got %d", len(ids))
		}
	})
}

func TestHub_AuthOk_FirstFrame(t *testing.T) {
	tests := []struct {
		name         string
		userID       string
		vehicleIDs   []string
		wantUserID   string
		wantVehCount int
	}{
		{
			name:         "single vehicle",
			userID:       "user-abc",
			vehicleIDs:   []string{"v-1"},
			wantUserID:   "user-abc",
			wantVehCount: 1,
		},
		{
			name:         "multiple vehicles",
			userID:       "user-xyz",
			vehicleIDs:   []string{"v-1", "v-2", "v-3"},
			wantUserID:   "user-xyz",
			wantVehCount: 3,
		},
		{
			name:         "no vehicles",
			userID:       "user-empty",
			vehicleIDs:   []string{},
			wantUserID:   "user-empty",
			wantVehCount: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := newTestHub(t)
			t.Cleanup(hub.Stop)

			auth := &testAuth{userID: tt.userID, vehicleIDs: tt.vehicleIDs}
			srv := newTestServer(t, hub, auth)
			t.Cleanup(srv.Close)

			conn := dialAndAuthRaw(t, srv.URL, "valid-token")
			defer conn.Close(websocket.StatusNormalClosure, "")

			// The FIRST frame must be auth_ok.
			msg := readMessage(t, conn)
			if msg.Type != msgTypeAuthOk {
				t.Fatalf("expected first frame type %q, got %q", msgTypeAuthOk, msg.Type)
			}

			var payload authOkPayload
			if err := json.Unmarshal(msg.Payload, &payload); err != nil {
				t.Fatalf("unmarshal auth_ok payload: %v", err)
			}

			if payload.UserID != tt.wantUserID {
				t.Errorf("userId = %q, want %q", payload.UserID, tt.wantUserID)
			}
			if payload.VehicleCount != tt.wantVehCount {
				t.Errorf("vehicleCount = %d, want %d", payload.VehicleCount, tt.wantVehCount)
			}

			// issuedAt must be a valid RFC3339 timestamp.
			if _, err := time.Parse(time.RFC3339, payload.IssuedAt); err != nil {
				t.Errorf("issuedAt %q is not valid RFC3339: %v", payload.IssuedAt, err)
			}
		})
	}
}

func TestHub_AuthOk_NotEmittedOnFailure(t *testing.T) {
	tests := []struct {
		name     string
		auth     *testAuth
		sendAuth bool // false = trigger auth_timeout by not sending auth
		wantCode string
	}{
		{
			name:     "auth_failed on invalid token",
			auth:     &testAuth{err: ErrInvalidToken},
			sendAuth: true,
			wantCode: errCodeAuthFailed,
		},
		{
			name:     "auth_timeout on no auth frame",
			auth:     &testAuth{userID: "user-1", vehicleIDs: []string{"v-1"}},
			sendAuth: false,
			wantCode: errCodeAuthTimeout,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := newTestHub(t)
			t.Cleanup(hub.Stop)

			handler := hub.Handler(tt.auth, HandlerConfig{
				AuthTimeout:  200 * time.Millisecond,
				WriteTimeout: 2 * time.Second,
			})
			srv := httptest.NewServer(handler)
			t.Cleanup(srv.Close)

			var conn *websocket.Conn
			if tt.sendAuth {
				conn = dialAndAuthRaw(t, srv.URL, "bad-token")
			} else {
				conn = dialOnly(t, srv.URL)
			}
			defer conn.Close(websocket.StatusNormalClosure, "")

			// Read whatever frame the server sends. It must be an error,
			// NOT auth_ok.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()

			_, data, err := conn.Read(ctx)
			if err != nil {
				// On timeout path the server may close the connection
				// before we read; that is acceptable — no auth_ok was sent.
				return
			}

			var msg wsMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}

			if msg.Type == msgTypeAuthOk {
				t.Fatal("auth_ok must NOT be emitted on failed auth paths")
			}
			if msg.Type != msgTypeError {
				t.Fatalf("expected error frame, got %q", msg.Type)
			}

			var errPl errorPayload
			if err := json.Unmarshal(msg.Payload, &errPl); err != nil {
				t.Fatalf("unmarshal error payload: %v", err)
			}
			if errPl.Code != tt.wantCode {
				t.Fatalf("error code = %q, want %q", errPl.Code, tt.wantCode)
			}
		})
	}
}
