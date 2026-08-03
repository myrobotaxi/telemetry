package ws

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/mask"
)

// MYR-435 — the WebSocket half of the both-surfaces proof.
//
// Client decision (2026-08-02): "Remove media/cabin and any vehicle controls."
// internal/mask pins the projection and internal/telemetry pins the REST
// snapshot; this file pins the frame a real subscribed client actually reads
// off the socket. It goes end-to-end through dial -> auth -> BroadcastMasked
// -> read rather than calling the mask directly, because the failure this
// guards against is a delivery path that forgets to consult the table at all
// (or consults it with the wrong resource type), which a mask-level test
// cannot see.
//
// Assertions are on RAW JSON, not on the decoded fields map: the contract is
// that the KEY is absent from the bytes (websocket-protocol.md §4.6,
// "absent, not nulled").

// narrowedFrameFields is a payload spanning all three removal groups plus the
// fields a viewer keeps, so one broadcast exercises the whole decision.
func narrowedFrameFields() map[string]any {
	return map[string]any{
		// Kept.
		"speed":       65,
		"latitude":    37.7749,
		"longitude":   -122.4194,
		"chargeLevel": 82,
		"status":      "driving",
		"lastUpdated": "2026-08-02T12:00:00Z",
		// Removed — media.
		"mediaNowPlayingTitle":      "Blood on the Tracks",
		"mediaNowPlayingArtist":     "Bob Dylan",
		"mediaNowPlayingAlbum":      "Tangled Up in Blue",
		"mediaNowPlayingStation":    "SiriusXM Deep Tracks",
		"mediaPlaybackSource":       "Spotify",
		"mediaPlaybackStatus":       "Playing",
		"mediaVolume":               7.5,
		"mediaVolumeMax":            11.0,
		"mediaNowPlayingDurationMs": 214000,
		"mediaNowPlayingElapsedMs":  42000,
		// Removed — cabin.
		"interiorTemp":    68,
		"exteriorTemp":    55,
		"isClimateOn":     true,
		"fanSpeed":        3,
		"seatHeaterLeft":  2,
		"seatVentEnabled": true,
		// Removed — controls.
		"locked":             true,
		"chargePortDoorOpen": false,
		"frunkOpen":          false,
		"trunkOpen":          true,
		"virtualKeyPaired":   true,
		// Removed — identity (MYR-279).
		"vin": "7SAYGDET7TA613795",
	}
}

// removedFrameKeys is every key from narrowedFrameFields a viewer must not see.
var removedFrameKeys = []string{
	"mediaNowPlayingTitle", "mediaNowPlayingArtist", "mediaNowPlayingAlbum",
	"mediaNowPlayingStation", "mediaPlaybackSource", "mediaPlaybackStatus",
	"mediaVolume", "mediaVolumeMax",
	"mediaNowPlayingDurationMs", "mediaNowPlayingElapsedMs",
	"interiorTemp", "exteriorTemp", "isClimateOn", "fanSpeed",
	"seatHeaterLeft", "seatVentEnabled",
	"locked", "chargePortDoorOpen", "frunkOpen", "trunkOpen", "virtualKeyPaired",
	"vin",
}

// connectRole dials a one-vehicle client holding the given role and returns the
// live connection.
func connectRole(t *testing.T, hub *Hub, role auth.Role) *websocket.Conn {
	t.Helper()

	a := &testAuth{
		userID:        "user-1",
		vehicleIDs:    []string{"v-1"},
		roleByVehicle: map[string]auth.Role{"v-1": role},
	}
	srv := newTestServer(t, hub, a)
	t.Cleanup(srv.Close)

	conn := dialAndAuth(t, srv.URL, "valid-token")
	t.Cleanup(func() { conn.Close(websocket.StatusNormalClosure, "") })

	waitForClients(t, hub, 1)
	return conn
}

// TestHub_BroadcastMasked_ViewerFrameOmitsMediaCabinAndControls is the headline
// WS assertion for MYR-435.
func TestHub_BroadcastMasked_ViewerFrameOmitsMediaCabinAndControls(t *testing.T) {
	hub := newTestHub(t)
	t.Cleanup(hub.Stop)

	conn := connectRole(t, hub, auth.RoleViewer)

	hub.BroadcastMasked(
		"v-1",
		mask.ResourceVehicleState,
		time.Now().UTC().Format(time.RFC3339),
		narrowedFrameFields(),
	)

	got := readMessage(t, conn)
	if got.Type != msgTypeVehicleUpdate {
		t.Fatalf("expected %q, got %q", msgTypeVehicleUpdate, got.Type)
	}

	// Raw-bytes check first: this catches a leak under ANY key name or
	// nesting, including a value echoed somewhere unexpected.
	raw := string(got.Payload)
	for _, secret := range []string{
		"Bob Dylan", "Blood on the Tracks", "Tangled Up in Blue",
		"SiriusXM Deep Tracks", "Spotify", "7SAYGDET7TA613795",
	} {
		if strings.Contains(raw, secret) {
			t.Errorf("viewer frame leaks %q: %s", secret, raw)
		}
	}

	var pl vehicleUpdatePayload
	if err := json.Unmarshal(got.Payload, &pl); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	for _, key := range removedFrameKeys {
		if _, present := pl.Fields[key]; present {
			t.Errorf("viewer frame carries %q — MYR-435 removes media, cabin state "+
				"and vehicle controls from viewers", key)
		}
	}

	// Absent, not nulled.
	if strings.Contains(raw, "null") {
		t.Errorf("viewer frame contains a null for a stripped key: %s", raw)
	}

	// The viewer still gets a usable frame.
	for _, key := range []string{"speed", "latitude", "longitude", "chargeLevel", "status"} {
		if _, present := pl.Fields[key]; !present {
			t.Errorf("viewer frame is missing %q — the narrowing removed too much", key)
		}
	}
}

// TestHub_BroadcastMasked_OwnerFrameUnchangedByMYR435 is the WS-side regression
// pin: the same broadcast reaches an OWNER with every field intact. MYR-435
// changed the viewer arm only, and an owner's own app must not lose its control
// tiles because sharing got stricter.
func TestHub_BroadcastMasked_OwnerFrameUnchangedByMYR435(t *testing.T) {
	hub := newTestHub(t)
	t.Cleanup(hub.Stop)

	conn := connectRole(t, hub, auth.RoleOwner)

	sent := narrowedFrameFields()
	hub.BroadcastMasked(
		"v-1",
		mask.ResourceVehicleState,
		time.Now().UTC().Format(time.RFC3339),
		sent,
	)

	got := readMessage(t, conn)
	var pl vehicleUpdatePayload
	if err := json.Unmarshal(got.Payload, &pl); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	if len(pl.Fields) != len(sent) {
		t.Errorf("owner frame has %d fields, want %d — the owner projection must be "+
			"the identity over the owner allow-list", len(pl.Fields), len(sent))
	}
	for key := range sent {
		if _, present := pl.Fields[key]; !present {
			t.Errorf("owner frame is missing %q — MYR-435 narrowed the VIEWER arm only", key)
		}
	}
}

// TestHub_BroadcastMasked_CabinOnlyFrameIsSuppressedForViewers pins the
// interaction between MYR-435 and the §4.6 empty-payload suppression rule,
// which this change makes load-bearing rather than theoretical.
//
// THIS TEST WAS DISHONEST IN ITS FIRST VERSION and the correction is the point.
// It broadcast a cabin/media payload with NO `lastUpdated` key — a shape
// production can never emit, because nav_broadcast.go injects `lastUpdated`
// into every non-nav frame before masking. So the test passed against a
// suppression gate (`len(projected) == 0`) that could not fire on real traffic:
// for a viewer the real payload projects to exactly one key, {"lastUpdated"},
// and the frame went out. The test's own doc comment named the hazard it was
// failing to cover.
//
// It now includes `lastUpdated` exactly as production does. The end-to-end
// version driving the real Broadcaster is
// TestBroadcaster_CabinOnlyTelemetry_SendsViewerNothing below; this one stays
// as the focused hub-level unit.
func TestHub_BroadcastMasked_CabinOnlyFrameIsSuppressedForViewers(t *testing.T) {
	hub := newTestHub(t)
	t.Cleanup(hub.Stop)

	conn := connectRole(t, hub, auth.RoleViewer)

	// Cabin/media/controls only — every field is owner-only after MYR-435 —
	// PLUS the lastUpdated key the production path always adds.
	hub.BroadcastMasked(
		"v-1",
		mask.ResourceVehicleState,
		time.Now().UTC().Format(time.RFC3339),
		map[string]any{
			"interiorTemp":             70,
			"isClimateOn":              true,
			"mediaNowPlayingTitle":     "Blood on the Tracks",
			"mediaNowPlayingElapsedMs": 42000,
			"locked":                   true,
			"lastUpdated":              time.Now().UTC().Format(time.RFC3339),
		},
	)

	// Then a frame the viewer IS allowed to see. If suppression works, this is
	// the FIRST frame that arrives; if it does not, the freshness-only cabin
	// frame arrives first and this assertion fails on the missing speed.
	hub.BroadcastMasked(
		"v-1",
		mask.ResourceVehicleState,
		time.Now().UTC().Format(time.RFC3339),
		map[string]any{
			"speed":       42,
			"lastUpdated": time.Now().UTC().Format(time.RFC3339),
		},
	)

	got := readMessage(t, conn)
	var pl vehicleUpdatePayload
	if err := json.Unmarshal(got.Payload, &pl); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if _, present := pl.Fields["speed"]; !present {
		t.Errorf("first frame the viewer received was not the speed frame (%v) — the "+
			"cabin-only frame was NOT suppressed, leaking activity timing", pl.Fields)
	}
}

// TestHub_BroadcastMasked_FreshnessOnlyProjectionIsSuppressed states the rule
// directly, independent of which owner-only fields happen to be in the payload:
// a viewer projection that survives as nothing but envelope keys is not a frame.
//
// Guards the generalization rather than the instance. A future envelope-ish key
// added to the viewer allow-list would re-open the hole for a payload shape this
// test does not enumerate, which is why mask.IsSubstantive owns the key set.
func TestHub_BroadcastMasked_FreshnessOnlyProjectionIsSuppressed(t *testing.T) {
	hub := newTestHub(t)
	t.Cleanup(hub.Stop)

	conn := connectRole(t, hub, auth.RoleViewer)

	// A single owner-only delta plus freshness — the minimal leak shape.
	hub.BroadcastMasked(
		"v-1",
		mask.ResourceVehicleState,
		time.Now().UTC().Format(time.RFC3339),
		map[string]any{
			"mediaNowPlayingElapsedMs": 42000,
			"lastUpdated":              "2026-08-02T12:00:00Z",
		},
	)
	hub.BroadcastMasked(
		"v-1",
		mask.ResourceVehicleState,
		time.Now().UTC().Format(time.RFC3339),
		map[string]any{"speed": 7},
	)

	got := readMessage(t, conn)
	var pl vehicleUpdatePayload
	if err := json.Unmarshal(got.Payload, &pl); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if _, present := pl.Fields["lastUpdated"]; present {
		if _, hasSpeed := pl.Fields["speed"]; !hasSpeed {
			t.Errorf("viewer received a freshness-only frame %v — a media tick becomes a "+
				"beacon that pulses only while audio plays, which is the occupancy signal "+
				"MYR-435 removes the media block to prevent", pl.Fields)
		}
	}
	if _, present := pl.Fields["speed"]; !present {
		t.Errorf("first viewer frame was %v, want the speed frame", pl.Fields)
	}
}
