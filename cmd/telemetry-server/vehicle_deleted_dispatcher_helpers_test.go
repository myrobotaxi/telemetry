package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/tnando/my-robo-taxi-telemetry/internal/auth"
	"github.com/tnando/my-robo-taxi-telemetry/internal/ws"
)

// fakeAuth is a minimal ws.Authenticator for the dispatcher
// integration test.
type fakeAuth struct {
	userID     string
	vehicleIDs []string
}

func (a *fakeAuth) ValidateToken(_ context.Context, token string) (string, error) {
	if token == "" {
		return "", auth.ErrInvalidToken
	}
	return a.userID, nil
}

func (a *fakeAuth) GetUserVehicles(_ context.Context, _ string) ([]string, error) {
	return a.vehicleIDs, nil
}

func (a *fakeAuth) ResolveRole(_ context.Context, _, _ string) (auth.Role, error) {
	return auth.RoleOwner, nil
}

func newWSTestServer(t *testing.T, hub *ws.Hub, a ws.Authenticator) *httptest.Server {
	t.Helper()
	handler := hub.Handler(a, ws.HandlerConfig{
		AuthTimeout:  2 * time.Second,
		WriteTimeout: 2 * time.Second,
	})
	return httptest.NewServer(handler)
}

func dialWSAuth(t *testing.T, baseURL, token string) *websocket.Conn {
	t.Helper()
	wsURL := strings.Replace(baseURL, "http://", "ws://", 1) + "/api/ws"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if resp != nil && resp.Body != nil {
		resp.Body.Close()
	}
	authMsg, _ := json.Marshal(map[string]any{
		"type":    "auth",
		"payload": json.RawMessage(`{"token":"` + token + `"}`),
	})
	if err := conn.Write(ctx, websocket.MessageText, authMsg); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Drain auth_ok.
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatalf("read auth_ok: %v", err)
	}
	return conn
}

func waitClients(t *testing.T, hub *ws.Hub, want int) {
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
			t.Fatalf("timed out waiting for %d clients", want)
		case <-tick.C:
		}
	}
}
