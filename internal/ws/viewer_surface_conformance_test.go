package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/mask"
)

// Per-surface viewer conformance (MYR-435, adversarial-review follow-up).
//
// These replaced a TAUTOLOGY in internal/mask —
// TestBothDeliverySurfacesShareTheVehicleStateTable compared
// For(ResourceVehicleState, role) against For(ResourceVehicleState, role), two
// identical calls that cannot disagree. It would have passed unchanged on a
// server where the REST handler and the WS hub consulted entirely different
// tables, because it never touched a delivery surface.
//
// Each test here drives ONE real surface and iterates
// mask.OwnerOnlyVehicleStateFields() — the authoritative table — rather than a
// hand-copied list, so no surface can quietly stop covering a field.
//
//	REST snapshot ........ internal/telemetry (sibling test)
//	WS live broadcast .... TestBroadcaster_CabinOnlyTelemetry_SendsViewerNothing
//	WS connect-time replay TestHub_SendSnapshot_ViewerReplayOmitsEveryOwnerOnlyField

// assertNoOwnerOnlyFields checks one delivered frame against the authoritative
// owner-only list.
func assertNoOwnerOnlyFields(t *testing.T, surface string, fields map[string]any) {
	t.Helper()
	for _, field := range mask.OwnerOnlyVehicleStateFields() {
		if _, present := fields[field]; present {
			t.Errorf("%s: viewer received owner-only field %q — MYR-435 withholds media, "+
				"cabin state and vehicle controls from viewers", surface, field)
		}
	}
}

// readFrameOrTimeout reads one message, reporting ok=false when the socket goes
// quiet. Distinct from readMessage, which t.Fatals on timeout — here "nothing
// arrived" is a legitimate and often desirable outcome.
func readFrameOrTimeout(t *testing.T, conn *websocket.Conn, wait time.Duration) (wsMessage, bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()

	_, data, err := conn.Read(ctx)
	if err != nil {
		return wsMessage{}, false
	}
	var msg wsMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return msg, true
}

// TestHub_SendSnapshot_ViewerReplayOmitsEveryOwnerOnlyField covers the WS
// connect-time snapshot REPLAY surface, which had no viewer test at all.
//
// It matters independently of the live broadcast: this is the one path where
// "the socket is masked" and "the snapshot is masked" must be the same
// statement, because it ships snapshot-sourced fields over the socket through
// enqueueSnapshotFrame rather than buildRoleFrames. Untested, a reconnecting
// viewer could be handed full cabin/media state on handshake, bypassing the
// live-path masking every other test covers.
func TestHub_SendSnapshot_ViewerReplayOmitsEveryOwnerOnlyField(t *testing.T) {
	fields := fullVehicleFields()
	for _, f := range mask.OwnerOnlyVehicleStateFields() {
		if _, exists := fields[f]; !exists {
			fields[f] = "SENSITIVE-" + f
		}
	}
	fields["mediaNowPlayingArtist"] = "Bob Dylan"
	fields["interiorTemp"] = 68

	reader := &fakeSnapshotReader{
		snapshots: map[string]VehicleSnapshot{
			"v-1": {Fields: fields, Timestamp: "2026-08-02T12:00:00Z"},
		},
	}
	hub := NewHub(slog.Default(), NoopHubMetrics{}, WithVehicleSnapshotReader(reader))
	t.Cleanup(hub.Stop)

	a := &testAuth{
		userID:        "user-1",
		vehicleIDs:    []string{"v-1"},
		roleByVehicle: map[string]auth.Role{"v-1": auth.RoleViewer},
	}
	srv := newTestServer(t, hub, a)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "token")
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Drain every replay frame. The count is deliberately not hard-coded: it
	// depends on how many atomic groups survive the viewer mask, which is the
	// very thing under test.
	var all []map[string]any
	for {
		msg, ok := readFrameOrTimeout(t, conn, 500*time.Millisecond)
		if !ok {
			break
		}
		all = append(all, vehicleUpdateFields(t, msg))
	}
	if len(all) == 0 {
		t.Fatal("viewer received no snapshot replay frames at all — a broken replay would " +
			"hide a leak rather than prove its absence")
	}

	for i, frame := range all {
		assertNoOwnerOnlyFields(t, "WS connect-time snapshot replay", frame)

		// No frame may consist solely of envelope keys. sendSnapshot sets
		// ungrouped["lastUpdated"] before projecting, so this path had the
		// identical latent hole the live path did.
		substantive := false
		for k := range frame {
			if !mask.IsEnvelopeField(k) {
				substantive = true
				break
			}
		}
		if !substantive {
			t.Errorf("replay frame %d to a viewer carries only envelope keys (%v) — a bare "+
				"freshness frame still signals that something changed", i, frame)
		}
	}

	sawKept := false
	for _, frame := range all {
		if _, ok := frame["model"]; ok {
			sawKept = true
		}
	}
	if !sawKept {
		t.Error("viewer replay carried no identity fields — the narrowing removed too much")
	}
}

// TestBroadcaster_CabinOnlyTelemetry_SendsViewerNothing is the end-to-end proof
// against the REAL production path — the test that would have caught the defect
// this review found.
//
// It publishes a genuine cabin-only telemetry event onto the event bus and lets
// Broadcaster.handleTelemetry do what it actually does, including injecting
// `lastUpdated` into the frame before masking. That injection is precisely what
// defeated the old `len(projected) == 0` suppression gate: the viewer projection
// was {"lastUpdated"}, length 1, so the gate never fired and the frame shipped.
// A hub-level test that hand-builds a payload without `lastUpdated` exercises a
// shape production cannot emit, and passed happily against the broken gate.
func TestBroadcaster_CabinOnlyTelemetry_SendsViewerNothing(t *testing.T) {
	bus := events.NewChannelBus(events.DefaultBusConfig(), events.NoopBusMetrics{}, slog.Default())
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	resolver := newStubVINResolver(map[string]string{"5YJ3E1EA1NF000001": "v-1"})

	hub := NewHub(slog.Default(), NoopHubMetrics{})
	t.Cleanup(hub.Stop)

	a := &testAuth{
		userID:        "user-1",
		vehicleIDs:    []string{"v-1"},
		roleByVehicle: map[string]auth.Role{"v-1": auth.RoleViewer},
	}
	srv := newTestServer(t, hub, a)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "valid-token")
	defer conn.Close(websocket.StatusNormalClosure, "")
	waitForClients(t, hub, 1)

	b := NewBroadcaster(hub, bus, resolver, slog.Default())
	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = b.Stop() })

	// A cabin-only tick: interior temperature plus a media elapsed counter.
	// Nothing here is viewer-visible, so the viewer must receive NOTHING — not
	// a frame carrying only lastUpdated.
	cabinOnly := events.NewEvent(events.VehicleTelemetryEvent{
		VIN:       "5YJ3E1EA1NF000001",
		CreatedAt: time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC),
		Fields: map[string]events.TelemetryValue{
			"insideTemp":               {FloatVal: ptrFloat64(72.0)},
			"mediaNowPlayingElapsedMs": {IntVal: ptrInt64(42000)},
		},
	})
	if err := bus.Publish(ctx, cabinOnly); err != nil {
		t.Fatalf("Publish cabin-only: %v", err)
	}

	// Then a speed tick the viewer IS entitled to. Both take the same immediate
	// non-nav path, so ordering holds: if the cabin frame was suppressed, the
	// FIRST frame the viewer sees is this one.
	speedOnly := events.NewEvent(events.VehicleTelemetryEvent{
		VIN:       "5YJ3E1EA1NF000001",
		CreatedAt: time.Date(2026, 8, 2, 12, 0, 1, 0, time.UTC),
		Fields: map[string]events.TelemetryValue{
			"speed": {FloatVal: ptrFloat64(65.0)},
		},
	})
	if err := bus.Publish(ctx, speedOnly); err != nil {
		t.Fatalf("Publish speed: %v", err)
	}

	got := vehicleUpdateFields(t, readMessage(t, conn))

	if _, hasSpeed := got["speed"]; !hasSpeed {
		t.Errorf("the first frame the viewer received was %v, not the speed frame. The "+
			"cabin-only tick was NOT suppressed: its values were masked but the FRAME "+
			"still went out, giving the viewer a beacon that pulses exactly when the "+
			"owner is in the cabin or playing audio", got)
	}
	assertNoOwnerOnlyFields(t, "WS live broadcast (real Broadcaster path)", got)
}

// TestBroadcaster_OwnerStillReceivesCabinOnlyTelemetry is the counterweight.
// Suppression must be a VIEWER outcome produced by masking, not a broadcaster
// that silently stopped forwarding cabin telemetry to everyone — which would
// pass every assertion above while breaking the owner's control tiles.
func TestBroadcaster_OwnerStillReceivesCabinOnlyTelemetry(t *testing.T) {
	bus := events.NewChannelBus(events.DefaultBusConfig(), events.NoopBusMetrics{}, slog.Default())
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	resolver := newStubVINResolver(map[string]string{"5YJ3E1EA1NF000001": "v-1"})

	hub := NewHub(slog.Default(), NoopHubMetrics{})
	t.Cleanup(hub.Stop)

	a := &testAuth{
		userID:        "user-1",
		vehicleIDs:    []string{"v-1"},
		roleByVehicle: map[string]auth.Role{"v-1": auth.RoleOwner},
	}
	srv := newTestServer(t, hub, a)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "valid-token")
	defer conn.Close(websocket.StatusNormalClosure, "")
	waitForClients(t, hub, 1)

	b := NewBroadcaster(hub, bus, resolver, slog.Default())
	ctx := context.Background()
	if err := b.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = b.Stop() })

	ev := events.NewEvent(events.VehicleTelemetryEvent{
		VIN:       "5YJ3E1EA1NF000001",
		CreatedAt: time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC),
		Fields: map[string]events.TelemetryValue{
			"insideTemp": {FloatVal: ptrFloat64(72.0)},
		},
	})
	if err := bus.Publish(ctx, ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	got := vehicleUpdateFields(t, readMessage(t, conn))
	if _, ok := got["interiorTemp"]; !ok {
		t.Errorf("owner did not receive the cabin-only frame (%v) — MYR-435 narrowed the "+
			"VIEWER arm; the owner's own telemetry must be untouched", got)
	}
}
