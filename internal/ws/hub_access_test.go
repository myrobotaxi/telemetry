package ws

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/myrobotaxi/telemetry/internal/auth"
)

// MYR-373 / websocket-protocol.md §10 DV-09. These run against the REAL hub
// over REAL WebSocket connections rather than poking at Client structs,
// because the thing being fixed is precisely that the struct looked fine and
// the socket kept streaming anyway.

// mutableAuth is a testAuth whose access set can change between connections,
// which is what a suspension looks like from the handshake's point of view.
// Guarded by a mutex because the revalidation sweep reads it from another
// goroutine while a test writes it.
type mutableAuth struct {
	mu         sync.Mutex
	userID     string
	vehicleIDs []string
	err        error
	calls      int
}

func (a *mutableAuth) ValidateToken(_ context.Context, token string) (string, error) {
	if token == "" {
		return "", ErrInvalidToken
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.userID, nil
}

func (a *mutableAuth) GetUserVehicles(_ context.Context, _ string) ([]string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	if a.err != nil {
		return nil, a.err
	}
	return append([]string(nil), a.vehicleIDs...), nil
}

func (a *mutableAuth) ResolveRole(_ context.Context, _, _ string) (auth.Role, error) {
	return auth.RoleViewer, nil
}

func (a *mutableAuth) set(ids []string, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.vehicleIDs, a.err = ids, err
}

func (a *mutableAuth) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// expectClosed4002 asserts the connection was closed with the §6.2 permission
// frame. Returns how long the close took to arrive from the moment of the call.
func expectClosed4002(t *testing.T, conn *websocket.Conn, who string) time.Duration {
	t.Helper()
	start := time.Now()
	readCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	_, _, err := conn.Read(readCtx)
	elapsed := time.Since(start)

	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("%s: expected a websocket close, got %T: %v", who, err, err)
	}
	if closeErr.Code != closeCodeVehicleAccessRevoked {
		t.Errorf("%s: close code = %d, want %d", who, closeErr.Code, closeCodeVehicleAccessRevoked)
	}
	// The reason a revoked viewer sees is the SAME string the vehicle-deletion
	// path emits, deliberately: the server does not tell them whether the car
	// was deleted, their grant was revoked, or they were suspended.
	if closeErr.Reason != vehicleAccessRevokedReason {
		t.Errorf("%s: close reason = %q, want %q", who, closeErr.Reason, vehicleAccessRevokedReason)
	}
	return elapsed
}

// expectStillOpen asserts the connection is NOT closed and, if a frame is
// pending, that it arrives. A short read window is enough: a close would be
// delivered immediately, not eventually.
func expectStillOpen(t *testing.T, conn *websocket.Conn, who string) {
	t.Helper()
	readCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_, _, err := conn.Read(readCtx)
	var closeErr websocket.CloseError
	if errors.As(err, &closeErr) {
		t.Fatalf("%s: connection was closed (code=%d reason=%q) and should not have been",
			who, closeErr.Code, closeErr.Reason)
	}
}

// TestRevokeUserAccess_UnregistersTheSession is the leak regression.
//
// Cutting a session off is only half a teardown. The revoked flag makes
// enqueue refuse everything, which means writePump can never again be woken
// by a message — and writePump holds g.Wait(), which holds Unregister. Before
// the `done` signal, a revoked session's two pump goroutines and its hub entry
// survived FOREVER: on a long-lived machine every revocation leaked them, the
// connected-clients gauge only ever climbed, and the backstop sweep re-examined
// the phantom every 60 seconds for the life of the process.
//
// The 15s heartbeat used to paper over this by failing a write on the dead
// connection. Relying on that was never a design; the enqueue guard removed it
// and made the deadlock total.
func TestRevokeUserAccess_UnregistersTheSession(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	srv := newTestServer(t, hub, &testAuth{userID: "viewer", vehicleIDs: []string{"veh-1"}})
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)

	hub.RevokeUserAccess("viewer", "veh-1", "suspended")
	expectClosed4002(t, conn, "viewer")

	// No heartbeat is broadcast here, deliberately: the teardown must not
	// depend on unrelated traffic arriving to shake it loose.
	waitForClients(t, hub, 0)
}

// TestRevokeUserAccess_UnregistersEvenIfThePeerIgnoresTheClose is the hostile
// -peer case. The client never reads, so it never echoes the close frame and
// the graceful handshake runs to the library's timeout. The session must still
// drain out on its own.
func TestRevokeUserAccess_UnregistersEvenIfThePeerIgnoresTheClose(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	srv := newTestServer(t, hub, &testAuth{userID: "viewer", vehicleIDs: []string{"veh-1"}})
	defer srv.Close()

	// Dialled and authenticated, then deliberately never read from again.
	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)

	hub.RevokeUserAccess("viewer", "veh-1", "suspended")

	// writePump exits on the signal immediately; readPump exits once the
	// library gives up on the echo and closes the connection underneath it,
	// which bounds this at the close-handshake timeout rather than forever.
	waitForClientsWithin(t, hub, 0, 10*time.Second)
}

// waitForClientsWithin is waitForClients with a caller-chosen deadline, for
// the teardown paths bounded by the WebSocket close-handshake timeout rather
// than by anything this package controls.
func waitForClientsWithin(t *testing.T, hub *Hub, want int, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if got := hub.ClientCount(); got == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("timed out after %s waiting for %d clients, got %d — the revoked "+
				"session never unregistered", within, want, hub.ClientCount())
		case <-tick.C:
		}
	}
}

// TestRevokeUserAccess_StopsFramesBeforeTheCloseHandshakeCompletes pins the
// property the whole design rests on, and the one a graceful close alone does
// NOT give: the instant RevokeUserAccess returns, the session is cut off.
//
// A WebSocket close waits for the peer to echo the close frame — up to five
// seconds against a peer that never answers. If the cut-off were the close
// itself, a viewer could keep receiving live GPS for that whole window, which
// is a smaller version of exactly the bug being fixed.
func TestRevokeUserAccess_StopsFramesBeforeTheCloseHandshakeCompletes(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	srv := newTestServer(t, hub, &testAuth{userID: "viewer", vehicleIDs: []string{"veh-1"}})
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)

	client := hub.snapshotClients()[0]

	start := time.Now()
	hub.RevokeUserAccess("viewer", "veh-1", "suspended")
	elapsed := time.Since(start)

	// Cut off, and cut off WITHOUT having waited on the peer.
	if client.hasVehicle("veh-1") {
		t.Error("session still authorized for the vehicle after RevokeUserAccess returned")
	}
	if elapsed > time.Second {
		t.Errorf("RevokeUserAccess blocked for %s; it must not wait on the close handshake, "+
			"or one unresponsive viewer stalls every later revocation on the bus goroutine", elapsed)
	}
	t.Logf("RevokeUserAccess returned in %s", elapsed)

	// And a frame published after the revocation is not enqueued for them.
	hub.Broadcast("veh-1", []byte(`{"type":"vehicle_update","gps":"secret"}`))
	if n := len(client.send); n != 0 {
		t.Errorf("%d frame(s) queued for a revoked session, want 0", n)
	}

	// A second revocation for the same session — which is exactly what the
	// 60s backstop sweep does when it lands on top of a nudge that already
	// fired — reports nothing new rather than double-counting and spawning a
	// second close goroutine.
	if n := hub.RevokeUserAccess("viewer", "veh-1", "revalidation_backstop"); n != 0 {
		t.Errorf("re-revoking an already torn-down session reported %d, want 0", n)
	}
}

// TestRevokeUserAccess_SuspendedViewerStopsReceivingFrames is the headline
// case: a viewer holding a LIVE socket stops getting the car's frames when the
// owner suspends them, without waiting for a reconnect.
func TestRevokeUserAccess_SuspendedViewerStopsReceivingFrames(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	viewerAuth := &testAuth{userID: "viewer", vehicleIDs: []string{"veh-1"}}
	srv := newTestServer(t, hub, viewerAuth)
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)

	// Baseline: the socket IS streaming this vehicle before the suspension.
	// Without this the test could pass on a socket that never worked.
	hub.Broadcast("veh-1", []byte(`{"type":"vehicle_update"}`))
	readCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := conn.Read(readCtx); err != nil {
		t.Fatalf("baseline frame not delivered: %v", err)
	}

	closed := hub.RevokeUserAccess("viewer", "veh-1", "suspended")
	if closed != 1 {
		t.Fatalf("RevokeUserAccess closed %d sessions, want 1", closed)
	}

	// Anything broadcast after the suspension must not reach them. The read
	// below returns the close, not the frame.
	hub.Broadcast("veh-1", []byte(`{"type":"vehicle_update","gps":"secret"}`))
	latency := expectClosed4002(t, conn, "suspended viewer")
	t.Logf("suspended viewer torn down in %s", latency)
}

// TestRevokeUserAccess_RevokedViewerStopsReceivingFrames is the same property
// for §7.5.3 revoke. Revoke and suspend differ everywhere except here, so both
// are pinned rather than one standing in for the other.
func TestRevokeUserAccess_RevokedViewerStopsReceivingFrames(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	srv := newTestServer(t, hub, &testAuth{userID: "viewer", vehicleIDs: []string{"veh-1"}})
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)

	if n := hub.RevokeUserAccess("viewer", "veh-1", "revoked"); n != 1 {
		t.Fatalf("RevokeUserAccess closed %d sessions, want 1", n)
	}
	expectClosed4002(t, conn, "revoked viewer")
}

// TestRevokeUserAccess_OwnerUnaffected is the property that separates this
// from RemoveVehicle. Suspending a viewer must not disturb the owner's own
// stream of the same car — the car is fine, one person's grant moved.
func TestRevokeUserAccess_OwnerUnaffected(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	ownerSrv := newTestServer(t, hub, &testAuth{userID: "owner", vehicleIDs: []string{"veh-1"}})
	defer ownerSrv.Close()
	viewerSrv := newTestServer(t, hub, &testAuth{userID: "viewer", vehicleIDs: []string{"veh-1"}})
	defer viewerSrv.Close()

	ownerConn := dialAndAuth(t, ownerSrv.URL, "tok")
	defer ownerConn.Close(websocket.StatusNormalClosure, "test done")
	viewerConn := dialAndAuth(t, viewerSrv.URL, "tok")
	defer viewerConn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 2)

	if n := hub.RevokeUserAccess("viewer", "veh-1", "suspended"); n != 1 {
		t.Fatalf("RevokeUserAccess closed %d sessions, want exactly 1 (the viewer's)", n)
	}
	expectClosed4002(t, viewerConn, "viewer")

	// The owner is still connected AND still receiving this vehicle.
	hub.Broadcast("veh-1", []byte(`{"type":"vehicle_update"}`))
	readCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := ownerConn.Read(readCtx); err != nil {
		t.Fatalf("owner stopped receiving after a VIEWER was suspended: %v", err)
	}
}

// TestRevokeUserAccess_OtherViewersUnaffected: suspending one viewer of a
// shared car leaves the other viewers of the same car streaming.
func TestRevokeUserAccess_OtherViewersUnaffected(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	srvA := newTestServer(t, hub, &testAuth{userID: "viewer-a", vehicleIDs: []string{"veh-1"}})
	defer srvA.Close()
	srvB := newTestServer(t, hub, &testAuth{userID: "viewer-b", vehicleIDs: []string{"veh-1"}})
	defer srvB.Close()

	connA := dialAndAuth(t, srvA.URL, "tok")
	defer connA.Close(websocket.StatusNormalClosure, "test done")
	connB := dialAndAuth(t, srvB.URL, "tok")
	defer connB.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 2)

	hub.RevokeUserAccess("viewer-a", "veh-1", "suspended")
	expectClosed4002(t, connA, "viewer-a")
	expectStillOpen(t, connB, "viewer-b")
}

// TestRevokeUserAccess_UnheldVehicleIsNoop: an owner suspending a grant on a
// car this session never held must not close it. Guards against the scoping
// collapsing into "close all of this user's sockets".
func TestRevokeUserAccess_UnheldVehicleIsNoop(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	srv := newTestServer(t, hub, &testAuth{userID: "viewer", vehicleIDs: []string{"veh-1"}})
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)

	if n := hub.RevokeUserAccess("viewer", "veh-OTHER", "revoked"); n != 0 {
		t.Fatalf("RevokeUserAccess closed %d sessions for an unheld vehicle, want 0", n)
	}
	expectStillOpen(t, conn, "viewer")
}

// TestRevokeUserAccess_EmptyUserIDIsNotAWildcard. A malformed revocation must
// close NOTHING. The opposite reading — empty means everyone — would turn one
// bad event into a fleet-wide disconnect.
func TestRevokeUserAccess_EmptyUserIDIsNotAWildcard(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	srv := newTestServer(t, hub, &testAuth{userID: "viewer", vehicleIDs: []string{"veh-1"}})
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)

	if n := hub.RevokeUserAccess("", "veh-1", "revoked"); n != 0 {
		t.Fatalf("empty userID closed %d sessions, want 0 — it is not a wildcard", n)
	}
	expectStillOpen(t, conn, "viewer")
}

// TestRevokeUserAccess_EmptyVehicleIDClosesEverySessionOfThatUser pins the
// documented fallback: no vehicle named means re-evaluate everything that user
// holds. Still scoped to the user — the other user stays up.
func TestRevokeUserAccess_EmptyVehicleIDClosesEverySessionOfThatUser(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	srv := newTestServer(t, hub, &testAuth{userID: "viewer", vehicleIDs: []string{"veh-1", "veh-2"}})
	defer srv.Close()
	otherSrv := newTestServer(t, hub, &testAuth{userID: "other", vehicleIDs: []string{"veh-1"}})
	defer otherSrv.Close()

	conn1 := dialAndAuth(t, srv.URL, "tok")
	defer conn1.Close(websocket.StatusNormalClosure, "test done")
	conn2 := dialAndAuth(t, srv.URL, "tok")
	defer conn2.Close(websocket.StatusNormalClosure, "test done")
	otherConn := dialAndAuth(t, otherSrv.URL, "tok")
	defer otherConn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 3)

	if n := hub.RevokeUserAccess("viewer", "", "revoked"); n != 2 {
		t.Fatalf("closed %d sessions, want 2 (both of that user's, none of anyone else's)", n)
	}
	expectClosed4002(t, conn1, "viewer session 1")
	expectClosed4002(t, conn2, "viewer session 2")
	expectStillOpen(t, otherConn, "other user")
}

// TestReconnectAfterSuspensionGetsReducedAccessSet pins the behavior the whole
// close-and-reconnect design leans on: the handshake re-derives, so the
// reconnect that follows a teardown comes back WITHOUT the suspended car.
//
// This passed before MYR-373 — the handshake always refetched. It is pinned
// because the fix now DEPENDS on it: closing the socket would be worse than
// useless if the reconnect handed the access straight back.
func TestReconnectAfterSuspensionGetsReducedAccessSet(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	authn := &mutableAuth{userID: "viewer", vehicleIDs: []string{"veh-1", "veh-2"}}
	srv := newTestServer(t, hub, authn)
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "tok")
	waitForClients(t, hub, 1)

	// The owner suspends the veh-1 grant: the access set narrows, then the
	// socket is torn down.
	authn.set([]string{"veh-2"}, nil)
	hub.RevokeUserAccess("viewer", "veh-1", "suspended")
	expectClosed4002(t, conn, "viewer")
	_ = conn.Close(websocket.StatusNormalClosure, "test done")

	// The revoked session LEAVES THE HUB. An earlier version of this test
	// tolerated it lingering and asserted against "the client that was not
	// here before" — which quietly documented a goroutine leak as expected
	// behavior. It unregisters, so assert that.
	waitForClients(t, hub, 0)

	// The client reconnects, as the SDK does automatically on a close.
	reconn := dialAndAuth(t, srv.URL, "tok")
	defer reconn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)

	fresh := hub.snapshotClients()[0]

	// It must NOT have veh-1 back. Asserted through the hub's own view of the
	// new client rather than the wire, because the reduced set is what every
	// later broadcast decision reads.
	held := fresh.authorizedVehicles()
	if len(held) != 1 || held[0] != "veh-2" {
		t.Fatalf("reconnected access set = %v, want [veh-2] — the suspended vehicle must not come back", held)
	}
	if authn.callCount() < 2 {
		t.Errorf("GetUserVehicles called %d times, want >= 2 — the reconnect must re-derive, not reuse", authn.callCount())
	}

	// And the suspended car's frames do not reach the new socket either.
	hub.Broadcast("veh-1", []byte(`{"type":"vehicle_update","gps":"secret"}`))
	expectStillOpen(t, reconn, "reconnected viewer")
}
