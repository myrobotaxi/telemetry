package main

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/ws"
)

// MYR-373 end to end through the REAL bus: the sharing handler's notifier call
// on one side, a real WebSocket client on the other, nothing stubbed in
// between. This is what pins the revocation latency the PR claims — the
// per-piece tests in internal/ws can only show the hub is fast once told.

func newShareAccessTestBus(t *testing.T, logger *slog.Logger) events.Bus {
	t.Helper()
	bus := events.NewChannelBus(events.BusConfig{}, events.NoopBusMetrics{}, logger)
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// awaitClose4002 waits for the connection to be closed with the §6.2
// permission frame and returns how long it took from start.
func awaitClose4002(t *testing.T, conn *websocket.Conn, start time.Time, who string) time.Duration {
	t.Helper()
	readCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, _, err := conn.Read(readCtx)
	elapsed := time.Since(start)

	var closeErr websocket.CloseError
	if !errors.As(err, &closeErr) {
		t.Fatalf("%s: expected a close frame, got %T: %v", who, err, err)
	}
	if closeErr.Code != 4002 {
		t.Fatalf("%s: close code = %d, want 4002", who, closeErr.Code)
	}
	return elapsed
}

// TestShareAccess_SuspendEndsLiveStream is the launch-blocker scenario: a
// viewer is watching a car live, the owner flips the suspend switch, and the
// stream must stop in seconds rather than at the next reconnect.
func TestShareAccess_SuspendEndsLiveStream(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	bus := newShareAccessTestBus(t, logger)

	hub := ws.NewHub(logger, &countingHubMetrics{})
	defer hub.Stop()

	if _, err := newShareAccessDispatcher(hub, logger).Subscribe(bus); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	notifier := newShareAccessBusNotifier(bus, logger)

	srv := newWSTestServer(t, hub, &fakeAuth{userID: "viewer", vehicleIDs: []string{"veh-shared"}})
	defer srv.Close()
	conn := dialWSAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitClients(t, hub, 1)

	// Exactly what ShareInviteHandler.ServePatch does after a suspending PATCH
	// commits and the grantee's cache is busted.
	start := time.Now()
	notifier.ShareAccessRevoked("viewer", "veh-shared", "suspended")

	latency := awaitClose4002(t, conn, start, "suspended viewer")
	if latency > 2*time.Second {
		t.Errorf("revocation took %s to reach the socket; the bound this fix claims is sub-second", latency)
	}
	t.Logf("suspend → live socket closed in %s", latency)
}

// TestShareAccess_RevokeEndsLiveStream: same path for §7.5.3 revoke.
func TestShareAccess_RevokeEndsLiveStream(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	bus := newShareAccessTestBus(t, logger)

	hub := ws.NewHub(logger, &countingHubMetrics{})
	defer hub.Stop()
	if _, err := newShareAccessDispatcher(hub, logger).Subscribe(bus); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	srv := newWSTestServer(t, hub, &fakeAuth{userID: "viewer", vehicleIDs: []string{"veh-shared"}})
	defer srv.Close()
	conn := dialWSAuth(t, srv.URL, "tok")
	defer conn.Close(websocket.StatusNormalClosure, "test done")
	waitClients(t, hub, 1)

	start := time.Now()
	newShareAccessBusNotifier(bus, logger).ShareAccessRevoked("viewer", "veh-shared", "revoked")

	t.Logf("revoke → live socket closed in %s", awaitClose4002(t, conn, start, "revoked viewer"))
}

// TestShareAccess_OwnerKeepsStreaming is the property that makes this a
// different pipeline from vehicle_deleted rather than a reuse of it. The car
// is not going away; one person's grant is.
func TestShareAccess_OwnerKeepsStreaming(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	bus := newShareAccessTestBus(t, logger)

	hub := ws.NewHub(logger, &countingHubMetrics{})
	defer hub.Stop()
	if _, err := newShareAccessDispatcher(hub, logger).Subscribe(bus); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	ownerSrv := newWSTestServer(t, hub, &fakeAuth{userID: "owner", vehicleIDs: []string{"veh-shared"}})
	defer ownerSrv.Close()
	viewerSrv := newWSTestServer(t, hub, &fakeAuth{userID: "viewer", vehicleIDs: []string{"veh-shared"}})
	defer viewerSrv.Close()

	ownerConn := dialWSAuth(t, ownerSrv.URL, "tok")
	defer ownerConn.Close(websocket.StatusNormalClosure, "test done")
	viewerConn := dialWSAuth(t, viewerSrv.URL, "tok")
	defer viewerConn.Close(websocket.StatusNormalClosure, "test done")
	waitClients(t, hub, 2)

	newShareAccessBusNotifier(bus, logger).ShareAccessRevoked("viewer", "veh-shared", "suspended")
	awaitClose4002(t, viewerConn, time.Now(), "viewer")

	// The owner is untouched and still receiving this car.
	hub.Broadcast("veh-shared", []byte(`{"type":"vehicle_update"}`))
	readCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, _, err := ownerConn.Read(readCtx); err != nil {
		t.Fatalf("the OWNER lost their stream when a viewer was suspended: %v", err)
	}
}

// TestShareAccessNotifier_IgnoresEmptyGrantee: a revocation that removed
// nothing (a pending invite, an idempotent second DELETE) must not publish. An
// empty grantee reaching the hub as a wildcard would be a self-inflicted
// disconnect storm.
func TestShareAccessNotifier_IgnoresEmptyGrantee(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))
	bus := newShareAccessTestBus(t, logger)

	received := make(chan events.Event, 1)
	if _, err := bus.Subscribe(events.TopicShareAccessRevoked, func(e events.Event) { received <- e }); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	newShareAccessBusNotifier(bus, logger).ShareAccessRevoked("", "veh-1", "revoked")

	select {
	case e := <-received:
		t.Fatalf("published an event for an empty grantee: %+v", e.Payload)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestShareAccessDispatcher_SurvivesMalformedInput. The dispatcher runs on the
// bus's per-subscription goroutine, so a panic here would not fail one
// revocation — it would silently kill EVERY later one on the same topic while
// the server carried on looking healthy. Each case must be an ordinary no-op.
func TestShareAccessDispatcher_SurvivesMalformedInput(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(discardWriter{}, nil))

	tests := []struct {
		name    string
		withHub bool
		evt     events.Event
	}{
		{
			name:    "a payload of the wrong type",
			withHub: true,
			evt:     events.NewEvent(events.VehicleDeletedEvent{VehicleID: "veh-1", UserID: "u"}),
		},
		{
			name:    "an empty grantee, which is not a wildcard",
			withHub: true,
			evt:     events.NewEvent(events.ShareAccessRevokedEvent{VehicleID: "veh-1", Reason: "revoked"}),
		},
		{
			name: "no hub wired at all",
			evt: events.NewEvent(events.ShareAccessRevokedEvent{
				GranteeUserID: "viewer", VehicleID: "veh-1", Reason: "revoked",
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var hub *ws.Hub
			if tt.withHub {
				hub = ws.NewHub(logger, &countingHubMetrics{})
				defer hub.Stop()
			}
			newShareAccessDispatcher(hub, logger).handle(tt.evt)
			if tt.withHub && hub.ClientCount() != 0 {
				t.Errorf("hub client count = %d, want 0", hub.ClientCount())
			}
		})
	}
}
