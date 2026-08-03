package ws

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The MYR-373 BACKSTOP. The event-driven nudge is the mechanism; this catches
// what the nudge structurally cannot — an event dropped by bus backpressure, a
// mutation served by another machine, a future write path that forgets to
// publish. Its correctness bar is therefore different: it must be right even
// when nobody told it anything, and it must never make things worse when the
// database is unhappy.

func TestAccessRevalidator_ClosesSessionThatLostAVehicle(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	authn := &mutableAuth{userID: "viewer", vehicleIDs: []string{"veh-1", "veh-2"}}
	srv := newTestServer(t, hub, authn)
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)

	// A suspension lands with nobody publishing anything — the exact scenario
	// the nudge misses.
	authn.set([]string{"veh-2"}, nil)

	rv := NewAccessRevalidator(hub, authn, time.Minute, newSilentLogger())
	if n := rv.SweepOnce(context.Background()); n != 1 {
		t.Fatalf("sweep closed %d sessions, want 1", n)
	}
	expectClosed4002(t, conn, "viewer who lost veh-1")
}

func TestAccessRevalidator_ClosesSessionThatLostEverything(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	authn := &mutableAuth{userID: "viewer", vehicleIDs: []string{"veh-1"}}
	srv := newTestServer(t, hub, authn)
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)

	// A revoked sole viewer: the access set goes EMPTY. An empty set is a real
	// answer — "you may see nothing" — and must be distinguished from the
	// wildcard, which also produces no per-vehicle entries.
	authn.set([]string{}, nil)

	if n := NewAccessRevalidator(hub, authn, 0, newSilentLogger()).SweepOnce(context.Background()); n != 1 {
		t.Fatalf("sweep closed %d sessions, want 1 for a user with no access left", n)
	}
	expectClosed4002(t, conn, "revoked sole viewer")
}

func TestAccessRevalidator_LeavesAnUnchangedSessionAlone(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	authn := &mutableAuth{userID: "viewer", vehicleIDs: []string{"veh-1"}}
	srv := newTestServer(t, hub, authn)
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)

	if n := NewAccessRevalidator(hub, authn, 0, newSilentLogger()).SweepOnce(context.Background()); n != 0 {
		t.Fatalf("sweep closed %d sessions with nothing changed, want 0", n)
	}
	expectStillOpen(t, conn, "unchanged viewer")
}

// TestAccessRevalidator_FailsOpenWhenResolveFails is the important negative.
// A sweep that fails CLOSED would convert a transient database error into a
// fleet-wide disconnect every interval — an outage caused by the safety net.
func TestAccessRevalidator_FailsOpenWhenResolveFails(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	authn := &mutableAuth{userID: "viewer", vehicleIDs: []string{"veh-1"}}
	srv := newTestServer(t, hub, authn)
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)

	authn.set(nil, errors.New("connection refused"))

	if n := NewAccessRevalidator(hub, authn, 0, newSilentLogger()).SweepOnce(context.Background()); n != 0 {
		t.Fatalf("sweep closed %d sessions on a resolver error, want 0 — it must fail OPEN", n)
	}
	expectStillOpen(t, conn, "viewer during a database blip")
}

// TestAccessRevalidator_SkipsWildcardClients: the dev-mode NoopAuthenticator
// reports all-access via the wildcard sentinel, which yields no per-vehicle
// entries. Reading that as "has lost everything" would disconnect every dev
// client once a minute.
func TestAccessRevalidator_SkipsWildcardClients(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	srv := newTestServer(t, hub, &NoopAuthenticator{UserID: "dev"})
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)

	rv := NewAccessRevalidator(hub, &NoopAuthenticator{UserID: "dev"}, 0, newSilentLogger())
	if n := rv.SweepOnce(context.Background()); n != 0 {
		t.Fatalf("sweep closed %d wildcard sessions, want 0", n)
	}
	expectStillOpen(t, conn, "dev-mode wildcard client")
}

// TestAccessRevalidator_ResolvesOncePerUser: three sessions for one person is
// one access question. Beyond the wasted queries, resolving per connection
// could answer differently mid-loop and close one tab while sparing another.
func TestAccessRevalidator_ResolvesOncePerUser(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	authn := &mutableAuth{userID: "viewer", vehicleIDs: []string{"veh-1"}}
	srv := newTestServer(t, hub, authn)
	defer srv.Close()

	for i := 0; i < 3; i++ {
		conn := dialAndAuth(t, srv.URL, "tok")
		defer conn.Close(websocket.StatusNormalClosure, "test done")
	}
	waitForClients(t, hub, 3)

	handshakeCalls := authn.callCount()
	NewAccessRevalidator(hub, authn, 0, newSilentLogger()).SweepOnce(context.Background())

	if got := authn.callCount() - handshakeCalls; got != 1 {
		t.Errorf("sweep made %d resolver calls for 3 sessions of one user, want 1", got)
	}
}

// TestAccessRevalidator_RunStopsOnContextCancel guards the goroutine wiring in
// main: a sweep loop that outlived shutdown would keep querying a closing pool.
func TestAccessRevalidator_RunStopsOnContextCancel(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	rv := NewAccessRevalidator(hub, &mutableAuth{userID: "u"}, 10*time.Millisecond, newSilentLogger())
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() { rv.Run(ctx); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// TestAccessRevalidator_RunWithoutDependenciesReturns: main skips the sweep in
// dev mode by not constructing it, but a nil resolver must not panic a
// goroutine if some future wiring passes one.
func TestAccessRevalidator_RunWithoutDependenciesReturns(t *testing.T) {
	done := make(chan struct{})
	go func() {
		NewAccessRevalidator(nil, nil, time.Millisecond, newSilentLogger()).
			Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run with nil dependencies did not return immediately")
	}
}
