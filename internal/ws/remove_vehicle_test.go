package ws

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// countingMetrics counts IncCloseUserDeletion calls for assertion.
type countingMetrics struct {
	NoopHubMetrics
	closeCalls atomic.Int32
}

func (m *countingMetrics) IncCloseUserDeletion() { m.closeCalls.Add(1) }

func newSilentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(discardW{}, nil))
}

type discardW struct{}

func (discardW) Write(p []byte) (int, error) { return len(p), nil }

// TestHub_RemoveVehicle_ClosesSubscribedClient connects a client to a
// real httptest WebSocket server, waits for hub.Register, calls
// RemoveVehicle, and asserts the client's next Read returns close
// code 4002 with the expected reason.
func TestHub_RemoveVehicle_ClosesSubscribedClient(t *testing.T) {
	metrics := &countingMetrics{}
	hub := NewHub(newSilentLogger(), metrics)
	defer hub.Stop()

	authn := &testAuth{userID: "user-1", vehicleIDs: []string{"veh-1"}}
	srv := newTestServer(t, hub, authn)
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")

	waitForClients(t, hub, 1)

	hub.RemoveVehicle("veh-1")

	readCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := conn.Read(readCtx)
	if err == nil {
		t.Fatal("expected close error after RemoveVehicle, got nil")
	}
	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("expected websocket.CloseError, got %T: %v", err, err)
	}
	if closeErr.Code != closeCodeVehicleAccessRevoked {
		t.Errorf("close code = %d, want %d", closeErr.Code, closeCodeVehicleAccessRevoked)
	}
	if closeErr.Reason != vehicleAccessRevokedReason {
		t.Errorf("close reason = %q, want %q", closeErr.Reason, vehicleAccessRevokedReason)
	}
	if got := metrics.closeCalls.Load(); got != 1 {
		t.Errorf("IncCloseUserDeletion = %d, want 1", got)
	}
}

// TestHub_RemoveVehicle_LeavesUnaffectedClient verifies the close path
// targets only clients authorized for the deleted vehicle.
func TestHub_RemoveVehicle_LeavesUnaffectedClient(t *testing.T) {
	metrics := &countingMetrics{}
	hub := NewHub(newSilentLogger(), metrics)
	defer hub.Stop()

	auth1 := &testAuth{userID: "user-1", vehicleIDs: []string{"veh-1"}}
	srv1 := newTestServer(t, hub, auth1)
	defer srv1.Close()
	auth2 := &testAuth{userID: "user-2", vehicleIDs: []string{"veh-2"}}
	srv2 := newTestServer(t, hub, auth2)
	defer srv2.Close()

	conn1 := dialAndAuth(t, srv1.URL, "tok")
	defer conn1.Close(websocket.StatusNormalClosure, "test done")
	conn2 := dialAndAuth(t, srv2.URL, "tok")
	defer conn2.Close(websocket.StatusNormalClosure, "test done")

	waitForClients(t, hub, 2)

	hub.RemoveVehicle("veh-1")

	readCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _, err := conn1.Read(readCtx)
	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != closeCodeVehicleAccessRevoked {
		t.Fatalf("conn1 expected close 4002, got: %v", err)
	}

	readCtx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	_, _, err2 := conn2.Read(readCtx2)
	if errors.As(err2, &closeErr) {
		t.Fatalf("conn2 unexpectedly closed: code=%d reason=%q", closeErr.Code, closeErr.Reason)
	}
	if got := metrics.closeCalls.Load(); got != 1 {
		t.Errorf("IncCloseUserDeletion = %d, want 1 (only conn1 affected)", got)
	}
}

// TestHub_RemoveVehicle_EmptyVehicleIDNoop verifies the empty-vehicleID guard.
func TestHub_RemoveVehicle_EmptyVehicleIDNoop(t *testing.T) {
	metrics := &countingMetrics{}
	hub := NewHub(newSilentLogger(), metrics)
	defer hub.Stop()
	hub.RemoveVehicle("")
	if got := metrics.closeCalls.Load(); got != 0 {
		t.Errorf("IncCloseUserDeletion = %d, want 0 for empty vehicleID", got)
	}
}

// TestHub_RemoveVehicle_NoMatchingClientNoop verifies a no-match call is a no-op.
func TestHub_RemoveVehicle_NoMatchingClientNoop(t *testing.T) {
	metrics := &countingMetrics{}
	hub := NewHub(newSilentLogger(), metrics)
	defer hub.Stop()
	hub.RemoveVehicle("does-not-exist")
	if got := metrics.closeCalls.Load(); got != 0 {
		t.Errorf("IncCloseUserDeletion = %d, want 0 (no clients connected)", got)
	}
}
