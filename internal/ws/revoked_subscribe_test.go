package ws

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// MYR-373 regression class: the CLIENT→SERVER path during the close window.
//
// The first cut of this fix guarded `hasVehicle` and claimed it was the single
// choke point. It was not. `subscribe` reaches `Hub.sendSnapshot` →
// `enqueueSnapshotFrame` → `Client.enqueue` without consulting `hasVehicle`
// even once, and `handleSubscribeFrame` gates on `owns()` — the
// HANDSHAKE-FROZEN vehicleIDs, which still contains the vehicle the owner just
// took away. An adversarial verifier reproduced live GPS `vehicle_update`
// frames delivered on the wire AFTER revocation and BEFORE the 4002 close,
// 25/25 attempts. Every test below exists because of that.

// gpsOnlySnapshot seeds the minimum that still produces a GPS frame, so the
// frame count on the wire is small and known: one ungrouped frame (carrying
// lastUpdated) plus one GPS frame.
func gpsOnlySnapshot() map[string]VehicleSnapshot {
	return map[string]VehicleSnapshot{
		"veh-1": {
			Fields: map[string]any{
				"latitude":  37.7749,
				"longitude": -122.4194,
				"heading":   90,
			},
			Timestamp: "2026-08-02T00:00:00Z",
		},
		"veh-2": {
			Fields:    map[string]any{"latitude": 1.0, "longitude": 2.0},
			Timestamp: "2026-08-02T00:00:00Z",
		},
	}
}

// snapshotFramesPerVehicle is how many frames one seeded snapshot produces:
// the ungrouped frame (carrying lastUpdated) plus one GPS group frame. Fixed
// by gpsOnlySnapshot above and verified against the wire, so the handshake
// drain below can be an exact count.
const snapshotFramesPerVehicle = 2

// drainHandshakeSnapshot consumes exactly the frames the handshake emits for a
// client holding ONE seeded vehicle, so a later assertion about "no frames" is
// about the window under test.
//
// An exact count, NOT a read-until-quiet loop: a Read whose context expires
// makes coder/websocket close the connection, so a timeout-based drain would
// destroy the very socket the test is about to use.
func drainHandshakeSnapshot(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	for range snapshotFramesPerVehicle {
		if msg := readMessage(t, conn); msg.Type != msgTypeVehicleUpdate {
			t.Fatalf("handshake drain: got %q, want %q", msg.Type, msgTypeVehicleUpdate)
		}
	}
}

// TestRevokedSessionCannotPullTelemetryViaSubscribe is the blocker regression,
// over a real socket against the real hub and its real readPump.
//
// IT SETS `revoked` DIRECTLY RATHER THAN CALLING RevokeUserAccess, and that is
// deliberate. The window under test is the interval between a session being
// cut off and its connection finishing dying — which is exactly when the peer
// can still get a `subscribe` serviced. Driving it through RevokeUserAccess
// makes the test RACE that window instead of occupying it: an earlier version
// of this test did exactly that and PASSED against the unguarded code, because
// the close usually won. A regression test that only sometimes reproduces the
// regression is not one. Setting the flag holds the window open, so the
// assertion is about the guard and nothing else.
//
// Verified to FAIL without the fix: the server delivers the GPS group frame
// (latitude/longitude/heading) straight back to the revoked viewer.
func TestRevokedSessionCannotPullTelemetryViaSubscribe(t *testing.T) {
	reader := &fakeSnapshotReader{snapshots: gpsOnlySnapshot()}
	hub := NewHub(newSilentLogger(), NoopHubMetrics{}, WithVehicleSnapshotReader(reader))
	defer hub.Stop()

	srv := newTestServer(t, hub, &testAuth{userID: "viewer", vehicleIDs: []string{"veh-1"}})
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)

	// Consume the handshake snapshot so what follows is only the window.
	drainHandshakeSnapshot(t, conn)

	// The session is cut off; its connection has not finished dying yet.
	hub.snapshotClients()[0].revoked.Store(true)

	// Try to pull the vehicle back in. owns() still says yes — the handshake
	// set is frozen and still contains it.
	writeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	mustWriteFrame(writeCtx, t, conn, msgTypeSubscribe, subscribePayload{VehicleID: "veh-1"})
	cancel()

	// Nothing may come back. A read that times out here is the PASS: the
	// deadline expiring means the server sent nothing, which is the point.
	readCtx, readCancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer readCancel()
	_, data, err := conn.Read(readCtx)
	if err != nil {
		return // silence, as required
	}

	var msg wsMessage
	if jerr := json.Unmarshal(data, &msg); jerr != nil {
		t.Fatalf("unmarshal: %v", jerr)
	}
	t.Fatalf("a REVOKED session pulled a %q frame via subscribe — live GPS "+
		"delivered after the owner cut them off: %s", msg.Type, string(msg.Payload))
}

// TestRevokedSessionGetsNoSnapshot covers the second unguarded window, which
// needs no peer cooperation at all: handler.go calls Register BEFORE the
// handshake's snapshot loop, so a revocation landing in between would find the
// client and then ship it the full snapshot anyway.
//
// Asserted directly against sendSnapshot because that ordering is internal to
// the handshake and cannot be driven from the wire.
func TestRevokedSessionGetsNoSnapshot(t *testing.T) {
	reader := &fakeSnapshotReader{snapshots: gpsOnlySnapshot()}
	hub := NewHub(newSilentLogger(), NoopHubMetrics{}, WithVehicleSnapshotReader(reader))
	defer hub.Stop()

	srv := newTestServer(t, hub, &testAuth{userID: "viewer", vehicleIDs: []string{"veh-1"}})
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)
	drainHandshakeSnapshot(t, conn)

	client := hub.snapshotClients()[0]
	client.revoked.Store(true)

	callsBefore := reader.calls
	hub.sendSnapshot(context.Background(), client, "veh-1", time.Second)

	if n := len(client.send); n != 0 {
		t.Errorf("%d snapshot frame(s) queued for a revoked session, want 0", n)
	}
	// And it did not even read the database for them.
	if reader.calls != callsBefore {
		t.Errorf("sendSnapshot fetched a snapshot for a revoked session (%d → %d calls)",
			callsBefore, reader.calls)
	}
}

// TestRevokedSessionEnqueueRefusesEveryFrame pins the choke point itself.
// enqueue is the ONLY writer to c.send, so a guard here is the version of
// "no further frames" that a newly added code path cannot bypass — which is
// exactly how the blocker got in.
//
// ASSERTED ON THE WIRE, NOT ON len(c.send). An earlier version checked the
// buffer depth and passed roughly half the time with the guard deleted,
// because writePump drains the channel within microseconds — the test was
// racing the very pump whose output it was trying to rule out. What the
// property actually says is "nothing reaches the peer", so that is what gets
// measured.
func TestRevokedSessionEnqueueRefusesEveryFrame(t *testing.T) {
	hub := NewHub(newSilentLogger(), NoopHubMetrics{})
	defer hub.Stop()

	srv := newTestServer(t, hub, &testAuth{userID: "viewer", vehicleIDs: []string{"veh-1"}})
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)

	client := hub.snapshotClients()[0]

	// revoked.Store, NOT markRevoked — AND THIS MATTERS. markRevoked also
	// signals writePump to exit, which would stop the frame for the wrong
	// reason and make this test pass with the enqueue guard deleted. The
	// window under test is "cut off, pumps still running", so only the flag
	// is set here.
	client.revoked.Store(true)

	if dropped := client.enqueue([]byte(`{"type":"vehicle_update","fields":{"latitude":37.7}}`)); dropped {
		t.Error("a refused frame reported itself as DROPPED; that inflates the " +
			"slow-client drop metric with revocations")
	}

	// Silence on the socket is the pass.
	readCtx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	if _, data, err := conn.Read(readCtx); err == nil {
		t.Fatalf("a frame reached a revoked session's peer: %s", string(data))
	}
}

// TestSubscribeDeniedKeepsTheConnectionOpen is the server half of the
// reconnect-loop tolerance (MYR-432 owns the client half).
//
// After a revocation the viewer reconnects, re-handshakes into the correctly
// reduced set, and re-sends the `subscribe` its stale local state still lists.
// On this NEW connection owns() is false. If the server answered that with a
// 4002 CLOSE — which it did before MYR-373 — the client would reconnect and do
// it again forever, a loop the SERVER drives. It must refuse the subscription
// and leave the session alone.
func TestSubscribeDeniedKeepsTheConnectionOpen(t *testing.T) {
	reader := &fakeSnapshotReader{snapshots: gpsOnlySnapshot()}
	hub := NewHub(newSilentLogger(), NoopHubMetrics{}, WithVehicleSnapshotReader(reader))
	defer hub.Stop()

	// The post-revocation reality: this user now holds veh-2 only.
	srv := newTestServer(t, hub, &testAuth{userID: "viewer", vehicleIDs: []string{"veh-2"}})
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)
	drainHandshakeSnapshot(t, conn)

	// Subscribe to the car they lost.
	writeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	mustWriteFrame(writeCtx, t, conn, msgTypeSubscribe, subscribePayload{VehicleID: "veh-1"})
	cancel()

	// A typed error frame, and NOT a close.
	msg := readMessage(t, conn)
	if msg.Type != msgTypeError {
		t.Fatalf("first frame = %q, want %q", msg.Type, msgTypeError)
	}

	// The session survives and still works for a vehicle they DO hold —
	// which is the whole point: refusing one subscription must not destroy a
	// connection that legitimately carries others.
	writeCtx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	mustWriteFrame(writeCtx2, t, conn, msgTypeSubscribe, subscribePayload{VehicleID: "veh-2"})
	cancel2()

	got := readMessage(t, conn)
	if got.Type != msgTypeVehicleUpdate {
		t.Fatalf("after a denied subscribe the connection stopped working: next frame = %q, want %q",
			got.Type, msgTypeVehicleUpdate)
	}
	if hub.ClientCount() != 1 {
		t.Errorf("client count = %d, want 1 — the denied subscribe closed the session", hub.ClientCount())
	}
}

// TestRevokedSessionSubscribeIsRefusedAtTheEntry pins the belt-and-braces
// guard: handleSubscribeFrame bails before owns() (which would say yes) and
// signals the readPump to exit, without emitting anything.
func TestRevokedSessionSubscribeIsRefusedAtTheEntry(t *testing.T) {
	reader := &fakeSnapshotReader{snapshots: gpsOnlySnapshot()}
	hub := NewHub(newSilentLogger(), NoopHubMetrics{}, WithVehicleSnapshotReader(reader))
	defer hub.Stop()

	srv := newTestServer(t, hub, &testAuth{userID: "viewer", vehicleIDs: []string{"veh-1"}})
	defer srv.Close()

	conn := dialAndAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitForClients(t, hub, 1)
	drainHandshakeSnapshot(t, conn)

	client := hub.snapshotClients()[0]
	client.revoked.Store(true)

	// owns() would still authorize this — that is the trap.
	if !client.owns("veh-1") {
		t.Fatal("precondition: the handshake set should still contain the revoked vehicle")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	keepOpen := client.handleSubscribeFrame(ctx, mustMarshalRaw(t, subscribePayload{VehicleID: "veh-1"}), time.Second)

	if keepOpen {
		t.Error("handleSubscribeFrame kept a revoked session's readPump alive")
	}
	if n := len(client.send); n != 0 {
		t.Errorf("%d frame(s) queued from a revoked session's subscribe, want 0", n)
	}
}
