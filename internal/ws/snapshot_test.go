package ws

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeSnapshotReader is a test double for VehicleSnapshotReader. err, when
// set, is returned for every vehicleID so tests can exercise the
// fetch-failure resilience path.
type fakeSnapshotReader struct {
	snapshots map[string]VehicleSnapshot
	err       error
	calls     int
}

func (f *fakeSnapshotReader) GetVehicleSnapshot(_ context.Context, vehicleID string) (VehicleSnapshot, error) {
	f.calls++
	if f.err != nil {
		return VehicleSnapshot{}, f.err
	}
	snap, ok := f.snapshots[vehicleID]
	if !ok {
		return VehicleSnapshot{}, errors.New("fakeSnapshotReader: no snapshot seeded for " + vehicleID)
	}
	return snap, nil
}

// fullVehicleFields returns a fields map with every wire field populated,
// mirroring what cmd/telemetry-server's wsVehicleSnapshotAdapter builds
// from a store.Vehicle row (adapters.go vehicleToSnapshotFields) for a
// parked vehicle with no active navigation.
func fullVehicleFields() map[string]any {
	return map[string]any{
		"name":                  "Stumpy",
		"model":                 "Model 3",
		"year":                  2024,
		"color":                 "Midnight Silver Metallic",
		"status":                "parked",
		"speed":                 0,
		"heading":               180,
		"latitude":              37.7749,
		"longitude":             -122.4194,
		"locationName":          "Home",
		"locationAddress":       "123 Market St, San Francisco, CA",
		"gearPosition":          nil,
		"chargeLevel":           78,
		"chargeState":           nil,
		"estimatedRange":        245,
		"timeToFull":            nil,
		"interiorTemp":          70,
		"exteriorTemp":          65,
		"odometerMiles":         12345,
		"fsdMilesSinceReset":    412.7,
		"destinationName":       nil,
		"destinationAddress":    nil,
		"destinationLatitude":   nil,
		"destinationLongitude":  nil,
		"originLatitude":        nil,
		"originLongitude":       nil,
		"etaMinutes":            nil,
		"tripDistanceRemaining": nil,
		"navRouteCoordinates":   nil,
	}
}

// TestPartitionSnapshotFields verifies the snapshot partitioner never
// mixes members of two different atomic groups into one bucket, matches
// the group membership tables in websocket-protocol.md §3.2, and routes
// every field not in an atomic group to `ungrouped` — including the
// MYR-137 fields (model, year, color, fsdMilesSinceReset).
func TestPartitionSnapshotFields(t *testing.T) {
	groups, ungrouped := partitionSnapshotFields(fullVehicleFields())

	wantGroups := map[atomicGroupID][]string{
		groupNavigation: {
			"destinationName", "destinationAddress",
			"destinationLatitude", "destinationLongitude",
			"originLatitude", "originLongitude",
			"etaMinutes", "tripDistanceRemaining", "navRouteCoordinates",
		},
		groupCharge: {"chargeLevel", "chargeState", "estimatedRange", "timeToFull"},
		groupGPS:    {"latitude", "longitude", "heading"},
		groupGear:   {"gearPosition", "status"},
	}

	for group, wantKeys := range wantGroups {
		got := groups[group]
		if len(got) != len(wantKeys) {
			t.Fatalf("group %s: got %d fields %v, want %d fields %v", group, len(got), keysOf(got), len(wantKeys), wantKeys)
		}
		for _, k := range wantKeys {
			if _, ok := got[k]; !ok {
				t.Errorf("group %s: missing expected member %q", group, k)
			}
		}
	}

	wantUngrouped := []string{
		"name", "model", "year", "color", "speed", "odometerMiles",
		"interiorTemp", "exteriorTemp", "fsdMilesSinceReset",
		"locationName", "locationAddress",
	}
	for _, k := range wantUngrouped {
		if _, ok := ungrouped[k]; !ok {
			t.Errorf("ungrouped: missing expected field %q", k)
		}
	}

	// No field should appear in more than one bucket.
	seen := map[string]string{}
	for group, fields := range groups {
		for k := range fields {
			if prev, ok := seen[k]; ok {
				t.Errorf("field %q appears in both %q and %q", k, prev, group)
			}
			seen[k] = string(group)
		}
	}
	for k := range ungrouped {
		if prev, ok := seen[k]; ok {
			t.Errorf("field %q appears in both ungrouped and %q", k, prev)
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// vehicleUpdateFields decodes a wsMessage's vehicle_update payload and
// returns its `fields` map, failing the test on any shape mismatch.
func vehicleUpdateFields(t *testing.T, msg wsMessage) map[string]any {
	t.Helper()
	if msg.Type != msgTypeVehicleUpdate {
		t.Fatalf("expected vehicle_update, got %q", msg.Type)
	}
	var payload vehicleUpdatePayload
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		t.Fatalf("unmarshal vehicle_update payload: %v", err)
	}
	return payload.Fields
}

// TestHub_SendSnapshot_OnHandshake covers MYR-137 acceptance criteria 1-3
// for the implicit auto-subscribe path: a client that never sends an
// explicit `subscribe` frame (the pre-MYR-46 SDK case) still receives the
// full persisted snapshot as one vehicle_update per atomic group
// immediately after auth_ok.
func TestHub_SendSnapshot_OnHandshake(t *testing.T) {
	reader := &fakeSnapshotReader{
		snapshots: map[string]VehicleSnapshot{
			"v-1": {Fields: fullVehicleFields(), Timestamp: "2026-07-05T12:00:00Z"},
		},
	}
	hub := NewHub(slog.Default(), NoopHubMetrics{}, WithVehicleSnapshotReader(reader))
	t.Cleanup(hub.Stop)

	auth := &testAuth{userID: "user-1", vehicleIDs: []string{"v-1"}}
	srv := newTestServer(t, hub, auth)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "token") // consumes auth_ok
	defer conn.Close(websocket.StatusNormalClosure, "")

	// Collect every vehicle_update frame delivered as part of the initial
	// snapshot (ungrouped + up to 4 atomic groups = 5 frames given
	// fullVehicleFields has content in every group).
	const wantFrames = 5
	var frames []map[string]any
	for i := 0; i < wantFrames; i++ {
		msg := readMessage(t, conn)
		frames = append(frames, vehicleUpdateFields(t, msg))
	}

	// AC1: model/year/color present at least once after subscribe.
	var catalogFrame map[string]any
	for _, f := range frames {
		if _, ok := f["model"]; ok {
			catalogFrame = f
			break
		}
	}
	if catalogFrame == nil {
		t.Fatal("no vehicle_update frame carried model/year/color")
	}
	for _, k := range []string{"model", "year", "color"} {
		if _, ok := catalogFrame[k]; !ok {
			t.Errorf("catalog frame missing %q", k)
		}
	}

	// AC3: fsdMilesSinceReset present (same ungrouped frame as the
	// catalog fields, since it's also an individual field).
	if got, ok := catalogFrame["fsdMilesSinceReset"]; !ok {
		t.Error("catalog frame missing fsdMilesSinceReset")
	} else if gotF, ok := got.(float64); !ok || gotF != 412.7 {
		t.Errorf("fsdMilesSinceReset = %v, want 412.7", got)
	}

	// AC2: estimatedRange included whenever chargeLevel is -- verify they
	// always land in the SAME frame (the charge atomic group), not just
	// somewhere in the stream.
	var chargeFrame map[string]any
	for _, f := range frames {
		if _, ok := f["chargeLevel"]; ok {
			chargeFrame = f
			break
		}
	}
	if chargeFrame == nil {
		t.Fatal("no vehicle_update frame carried chargeLevel")
	}
	if _, ok := chargeFrame["estimatedRange"]; !ok {
		t.Error("frame carrying chargeLevel did not also carry estimatedRange (charge atomic group split across frames)")
	}

	// Atomic-group rule (websocket-protocol.md §3.2): no single frame may
	// contain members of two different atomic groups.
	memberOf := map[string]atomicGroupID{}
	for group, names := range wireGroupMembers {
		for _, n := range names {
			memberOf[n] = group
		}
	}
	for _, f := range frames {
		var groupsInFrame []atomicGroupID
		seen := map[atomicGroupID]bool{}
		for k := range f {
			if g, ok := memberOf[k]; ok && !seen[g] {
				seen[g] = true
				groupsInFrame = append(groupsInFrame, g)
			}
		}
		if len(groupsInFrame) > 1 {
			t.Errorf("frame %v mixes atomic groups %v (contract violation)", f, groupsInFrame)
		}
	}

	// No extra frames beyond the expected 5 within a short window.
	readCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, _, err := conn.Read(readCtx); err == nil {
		t.Error("received an unexpected extra frame after the snapshot")
	}
}

// TestHub_SendSnapshot_OnExplicitSubscribe covers the MYR-46 explicit
// control-frame path: sending `subscribe` for an already-owned vehicle
// re-delivers the snapshot, so a client that re-subscribes (e.g. after
// toggling a vehicle off and back on in the UI) is not left waiting on
// live telemetry either.
func TestHub_SendSnapshot_OnExplicitSubscribe(t *testing.T) {
	reader := &fakeSnapshotReader{
		snapshots: map[string]VehicleSnapshot{
			"v-1": {Fields: fullVehicleFields(), Timestamp: "2026-07-05T12:00:00Z"},
		},
	}
	hub := NewHub(slog.Default(), NoopHubMetrics{}, WithVehicleSnapshotReader(reader))
	t.Cleanup(hub.Stop)

	auth := &testAuth{userID: "user-1", vehicleIDs: []string{"v-1"}}
	srv := newTestServer(t, hub, auth)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "token")
	defer conn.Close(websocket.StatusNormalClosure, "")
	waitForClients(t, hub, 1)

	// Drain the 5 handshake-time snapshot frames.
	for i := 0; i < 5; i++ {
		readMessage(t, conn)
	}

	callsBeforeSubscribe := reader.calls

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	mustWriteFrame(ctx, t, conn, msgTypeSubscribe, subscribePayload{VehicleID: "v-1"})

	// The explicit subscribe re-fetches and re-delivers the snapshot: at
	// least one more vehicle_update frame carrying model must arrive.
	found := false
	for i := 0; i < 5; i++ {
		msg := readMessage(t, conn)
		fields := vehicleUpdateFields(t, msg)
		if _, ok := fields["model"]; ok {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("explicit subscribe did not re-deliver a snapshot frame carrying model")
	}
	if reader.calls <= callsBeforeSubscribe {
		t.Error("expected GetVehicleSnapshot to be called again on explicit subscribe")
	}
}

// TestHub_SendSnapshot_FetchErrorIsNonFatal ensures a snapshot fetch
// failure (e.g. a transient DB error) does not break the connection --
// the client still gets auth_ok and can still receive live telemetry.
func TestHub_SendSnapshot_FetchErrorIsNonFatal(t *testing.T) {
	reader := &fakeSnapshotReader{err: errors.New("boom")}
	hub := NewHub(slog.Default(), NoopHubMetrics{}, WithVehicleSnapshotReader(reader))
	t.Cleanup(hub.Stop)

	auth := &testAuth{userID: "user-1", vehicleIDs: []string{"v-1"}}
	srv := newTestServer(t, hub, auth)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "token") // succeeds despite the fetch error
	defer conn.Close(websocket.StatusNormalClosure, "")
	waitForClients(t, hub, 1)

	// Live telemetry still works — do this positive (must-deliver)
	// assertion first. Per TestControlFrames_UnsubscribeStopsBroadcast's
	// note, coder/websocket closes the read side once a Read's context is
	// cancelled, so a "no frame arrives" negative check must never run
	// before a later read that expects to succeed.
	msg := mustMarshalTest(t, wsMessage{
		Type:    msgTypeDriveStarted,
		Payload: mustMarshalRaw(t, map[string]any{"vehicleId": "v-1"}),
	})
	hub.Broadcast("v-1", msg)

	deliverCtx, deliverCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer deliverCancel()
	_, data, err := conn.Read(deliverCtx)
	if err != nil {
		t.Fatalf("expected live broadcast to still reach the client after snapshot fetch failure, got %v", err)
	}

	// The first (and only) frame received after auth_ok must be the live
	// drive_started broadcast, not a snapshot vehicle_update -- proving
	// the failed fetch produced zero snapshot frames rather than a
	// delayed or malformed one racing the broadcast.
	var got wsMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != msgTypeDriveStarted {
		t.Fatalf("expected first post-auth_ok frame to be %q (no snapshot frames), got %q", msgTypeDriveStarted, got.Type)
	}
}

// TestHub_SendSnapshot_NilReaderIsNoop documents and locks in the
// pre-MYR-137 fallback: a hub without WithVehicleSnapshotReader configured
// never sends snapshot frames, matching every existing test's wiring.
func TestHub_SendSnapshot_NilReaderIsNoop(t *testing.T) {
	hub := newTestHub(t) // no snapshot reader configured
	t.Cleanup(hub.Stop)

	auth := &testAuth{userID: "user-1", vehicleIDs: []string{"v-1"}}
	srv := newTestServer(t, hub, auth)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "token")
	defer conn.Close(websocket.StatusNormalClosure, "")
	waitForClients(t, hub, 1)

	readCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, _, err := conn.Read(readCtx); err == nil {
		t.Error("expected no vehicle_update frames when no VehicleSnapshotReader is configured")
	}
}
