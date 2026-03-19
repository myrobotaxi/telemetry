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

// dialAndAuth connects to the test server and sends an auth message.
func dialAndAuth(t *testing.T, url, token string) *websocket.Conn {
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
	defer hub.Stop()

	auth := &testAuth{userID: "user-1", vehicleIDs: []string{"v-1", "v-2"}}
	srv := newTestServer(t, hub, auth)
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "valid-token")
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Give the server a moment to register the client.
	time.Sleep(50 * time.Millisecond)

	if got := hub.ClientCount(); got != 1 {
		t.Fatalf("expected 1 client, got %d", got)
	}
}

func TestHub_AuthFailure_InvalidToken(t *testing.T) {
	hub := newTestHub(t)
	defer hub.Stop()

	auth := &testAuth{err: ErrInvalidToken}
	srv := newTestServer(t, hub, auth)
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "bad-token")
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
	defer hub.Stop()

	auth := &testAuth{userID: "user-1", vehicleIDs: []string{"v-1"}}
	handler := hub.Handler(auth, HandlerConfig{
		AuthTimeout:  200 * time.Millisecond,
		WriteTimeout: 2 * time.Second,
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

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
	defer hub.Stop()

	// Client 1 sees vehicle v-1.
	auth1 := &testAuth{userID: "user-1", vehicleIDs: []string{"v-1"}}
	srv1 := newTestServer(t, hub, auth1)
	defer srv1.Close()
	conn1 := dialAndAuth(t, srv1.URL, "token-1")
	defer conn1.Close(websocket.StatusNormalClosure, "")

	// Client 2 sees vehicle v-2.
	auth2 := &testAuth{userID: "user-2", vehicleIDs: []string{"v-2"}}
	srv2 := newTestServer(t, hub, auth2)
	defer srv2.Close()
	conn2 := dialAndAuth(t, srv2.URL, "token-2")
	defer conn2.Close(websocket.StatusNormalClosure, "")

	time.Sleep(50 * time.Millisecond)

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
	defer hub.Stop()

	auth := &testAuth{userID: "user-1", vehicleIDs: []string{"v-1"}}
	srv := newTestServer(t, hub, auth)
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "token")
	defer conn.Close(websocket.StatusNormalClosure, "")

	time.Sleep(50 * time.Millisecond)

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
	defer hub.Stop()

	auth := &testAuth{userID: "slow-user", vehicleIDs: []string{"v-1"}}
	srv := newTestServer(t, hub, auth)
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "token")
	defer conn.Close(websocket.StatusNormalClosure, "")

	time.Sleep(50 * time.Millisecond)

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
	defer hub.Stop()

	auth := &testAuth{userID: "user-shared", vehicleIDs: []string{"v-shared"}}
	srv := newTestServer(t, hub, auth)
	defer srv.Close()

	const numClients = 3
	conns := make([]*websocket.Conn, numClients)
	for i := range numClients {
		conns[i] = dialAndAuth(t, srv.URL, "token")
		defer conns[i].Close(websocket.StatusNormalClosure, "")
	}

	time.Sleep(50 * time.Millisecond)

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
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "token")
	defer conn.Close(websocket.StatusNormalClosure, "")

	time.Sleep(50 * time.Millisecond)

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
	defer hub.Stop()

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
			name:       "empty list",
			vehicleIDs: nil,
			query:      "v-1",
			want:       false,
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
