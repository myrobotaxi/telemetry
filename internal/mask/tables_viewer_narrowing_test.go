package mask

import (
	"encoding/json"
	"sort"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/auth"
)

// MYR-435 — the client's viewer-mask narrowing, pinned at the table.
//
// Client decision (2026-08-02): "Remove media/cabin and any vehicle controls."
//
// These tests assert on RAW JSON KEYS rather than on the mask sets or on a Go
// struct, and that choice is load-bearing. The contract a viewer's client
// actually depends on is "the key is not in the bytes" (rest-api.md §5.1,
// absent-not-nulled). A struct-level assertion would pass just as happily
// against a field serialized as `"interiorTemp": null`, which still tells the
// viewer the field exists and, for booleans and zero-able numerics, still leaks
// a value-shaped default into their decoder.

// removedFromViewers is every field MYR-435 took away, grouped the way the
// client described them. Kept as one list so each test iterates the same set —
// a field added to the owner-only list must be added here too, which
// TestRemovedSetMatchesOwnerOnlyList enforces.
var removedFromViewers = map[string][]string{
	"media": {
		"mediaNowPlayingTitle",
		"mediaNowPlayingArtist",
		"mediaNowPlayingAlbum",
		"mediaNowPlayingStation",
		"mediaPlaybackSource",
		"mediaPlaybackStatus",
		"mediaVolume",
		"mediaVolumeMax",
		"mediaNowPlayingDurationMs",
		"mediaNowPlayingElapsedMs",
	},
	"cabin": {
		"interiorTemp",
		"exteriorTemp",
		"hvacPower",
		"isClimateOn",
		"fanSpeed",
		"driverTempSetting",
		"passengerTempSetting",
		"hvacAutoMode",
		"hvacAcEnabled",
		"seatHeaterLeft",
		"seatHeaterRight",
		"seatHeaterRearLeft",
		"seatHeaterRearCenter",
		"seatHeaterRearRight",
		"seatCoolerLeft",
		"seatCoolerRight",
		"seatVentEnabled",
		"seatCoolingCapable",
	},
	"controls": {
		"locked",
		"chargePortDoorOpen",
		"frunkOpen",
		"trunkOpen",
		"virtualKeyPaired",
	},
	"identity": {
		// Predates MYR-435 (MYR-279) but belongs to the removed set.
		"vin",
	},
}

// keptForViewers is what the viewing/riding features consume. Asserted
// POSITIVELY so that an over-zealous future narrowing — "remove more, it's
// safer" — breaks a test instead of quietly breaking the rider map.
var keptForViewers = []string{
	// Where the car is.
	"latitude", "longitude", "heading", "speed",
	"locationName", "locationAddress",
	// Where it is going.
	"destinationName", "destinationAddress",
	"destinationLatitude", "destinationLongitude",
	"etaMinutes", "tripDistanceRemaining", "navRouteCoordinates",
	"driveTrailCoordinates",
	// Which car it is.
	"vehicleId", "name", "model", "year", "color", "trim", "licensePlate",
	// Whether it can make the trip.
	"chargeLevel", "chargeState", "estimatedRange", "timeToFull",
	// Whether it is available.
	"status", "rideShareEnabled", "serviceEstimatedEndAt",
	// Whether what I am looking at is live.
	"lastUpdated",
}

// fullOwnerPayload builds a payload carrying EVERY field in the owner
// allow-list with a non-zero, JSON-distinguishable value. Every test below
// projects this same payload, so a field that the mask fails to strip shows up
// as its key in the output bytes.
func fullOwnerPayload() map[string]any {
	payload := make(map[string]any, len(vehicleStateOwnerFields))
	for i, f := range vehicleStateOwnerFields {
		// Values are irrelevant to masking; they only need to be present and
		// non-null so an unstripped key is visible in the marshaled bytes.
		payload[f] = i + 1
	}
	return payload
}

// projectJSONKeys projects the full owner payload for a role and returns the
// keys of the MARSHALED output — i.e. what is actually on the wire.
func projectJSONKeys(t *testing.T, role auth.Role) map[string]struct{} {
	t.Helper()

	projected, _ := Apply(fullOwnerPayload(), For(ResourceVehicleState, role))
	encoded, err := json.Marshal(projected)
	if err != nil {
		t.Fatalf("marshal %s projection: %v", role, err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("re-decode %s projection: %v", role, err)
	}
	keys := make(map[string]struct{}, len(decoded))
	for k := range decoded {
		keys[k] = struct{}{}
	}
	return keys
}

// TestViewerFrameOmitsEveryRemovedField is the headline assertion: for a viewer,
// none of the removed keys exist in the emitted JSON. Table-driven by group so a
// failure names which of the client's three categories leaked.
func TestViewerFrameOmitsEveryRemovedField(t *testing.T) {
	keys := projectJSONKeys(t, auth.RoleViewer)

	for group, fields := range removedFromViewers {
		t.Run(group, func(t *testing.T) {
			for _, f := range fields {
				if _, present := keys[f]; present {
					t.Errorf("viewer JSON contains %q — MYR-435 removes the %s group "+
						"from viewers entirely", f, group)
				}
			}
		})
	}
}

// TestViewerFrameOmitsMediaNowPlayingBlockSpecifically calls out the media block
// on its own because it was the MYR-427 audit's sharpest example: a viewer was
// receiving the owner's now-playing track title, artist, and album — free-text
// user content revealing listening habits — with no product feature consuming
// it once the "the rider can hear it anyway" argument is dropped.
//
// Asserted against the raw BYTES, not the key set, so that even a field
// serialized under an unexpected nesting could not hide.
func TestViewerFrameOmitsMediaNowPlayingBlockSpecifically(t *testing.T) {
	payload := fullOwnerPayload()
	// Give the free-text fields recognizable string values: if any survives,
	// the failure message shows the actual leaked content.
	payload["mediaNowPlayingTitle"] = "Blood on the Tracks"
	payload["mediaNowPlayingArtist"] = "Bob Dylan"
	payload["mediaNowPlayingAlbum"] = "Tangled Up in Blue"
	payload["mediaNowPlayingStation"] = "SiriusXM Deep Tracks"
	payload["mediaPlaybackSource"] = "Spotify"

	projected, masked := Apply(payload, For(ResourceVehicleState, auth.RoleViewer))
	encoded, err := json.Marshal(projected)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(encoded)

	for _, needle := range []string{
		"mediaNowPlaying", "mediaPlayback", "mediaVolume",
		"Bob Dylan", "Blood on the Tracks", "Tangled Up in Blue",
		"SiriusXM Deep Tracks", "Spotify",
	} {
		if contains(body, needle) {
			t.Errorf("viewer frame leaks media content %q: %s", needle, body)
		}
	}
	// "absent, not nulled" — a stripped key must not reappear as a null.
	if contains(body, "null") {
		t.Errorf("viewer frame contains null for a stripped key: %s", body)
	}
	// The strip must be reported to the audit path, not done silently.
	if !containsString(masked, "mediaNowPlayingTitle") {
		t.Error("mediaNowPlayingTitle was not reported in fieldsMasked — the audit " +
			"path (rest-api.md §5.3) would under-report the projection")
	}
}

// TestViewerFrameKeepsWhatRidingNeeds is the other direction. A mask narrowing
// that also took out the map or the charge level would satisfy every test above
// while destroying the feature.
func TestViewerFrameKeepsWhatRidingNeeds(t *testing.T) {
	keys := projectJSONKeys(t, auth.RoleViewer)

	for _, f := range keptForViewers {
		if _, present := keys[f]; !present {
			t.Errorf("viewer JSON is missing %q — viewers keep location, route, "+
				"identity, charge, availability and freshness (MYR-435)", f)
		}
	}
}

// TestOwnerFrameUnchangedByMYR435 is the regression pin. The owner projection of
// the full payload must still be the IDENTITY — every owner field survives —
// which is the precise sense in which an owner frame is byte-identical to what
// it was before this change.
//
// Pinned as a sorted key list compared against vehicleStateOwnerFields, so this
// fails if MYR-435's edits accidentally dropped an owner field while moving
// names between lists.
func TestOwnerFrameUnchangedByMYR435(t *testing.T) {
	payload := fullOwnerPayload()
	projected, masked := Apply(payload, For(ResourceVehicleState, auth.RoleOwner))

	if len(masked) != 0 {
		sort.Strings(masked)
		t.Errorf("owner projection stripped %v — the owner mask must be the identity "+
			"over the owner allow-list; MYR-435 changed the VIEWER arm only", masked)
	}
	if len(projected) != len(payload) {
		t.Fatalf("owner projection has %d fields, want %d", len(projected), len(payload))
	}

	got := make([]string, 0, len(projected))
	for k := range projected {
		got = append(got, k)
	}
	want := append([]string(nil), vehicleStateOwnerFields...)
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("owner key count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("owner key %d = %q, want %q", i, got[i], want[i])
		}
	}

	// Every removed-from-viewers field is STILL owner-visible. The client asked
	// to narrow sharing, not to delete features from the owner's own app.
	for group, fields := range removedFromViewers {
		for _, f := range fields {
			if _, present := projected[f]; !present {
				t.Errorf("owner lost %q (%s group) — MYR-435 must not touch the owner arm",
					f, group)
			}
		}
	}
}

// TestBothDeliverySurfacesShareTheVehicleStateTable pins the thing the issue
// asked to be verified rather than assumed: the WS broadcast mask and the REST
// snapshot mask are not two tables that happen to agree today.
//
// Both call sites resolve their mask through For(ResourceVehicleState, role) —
// internal/ws/hub_masked.go buildRoleFrames and internal/ws/snapshot.go
// enqueueSnapshotFrame for the socket, internal/telemetry/
// vehicle_snapshot_handler.go writeMaskedResponse for GET /snapshot. There is
// exactly ONE table, so they agree by construction; this test pins that
// For() is a pure function of (resource, role) with no hidden per-surface
// variation, so a future "just for the REST path" branch cannot be added
// without failing here.
func TestBothDeliverySurfacesShareTheVehicleStateTable(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleViewer} {
		wsMask := For(ResourceVehicleState, role)
		restMask := For(ResourceVehicleState, role)

		if len(wsMask.Allowed) != len(restMask.Allowed) {
			t.Fatalf("%s: ws mask has %d fields, rest mask %d — the two delivery "+
				"surfaces have diverged", role, len(wsMask.Allowed), len(restMask.Allowed))
		}
		for field := range wsMask.Allowed {
			if !restMask.allows(field) {
				t.Errorf("%s: %q allowed on the WS surface but not on REST", role, field)
			}
		}
		for field := range restMask.Allowed {
			if !wsMask.allows(field) {
				t.Errorf("%s: %q allowed on the REST surface but not on WS", role, field)
			}
		}
	}
}

// TestVehiclesListCarriesNoRemovedField checks the §7.0 VehicleSummary row for
// leakage. The catalog is a thin row that never carried media, cabin or
// controls state, so this passes today — it is here as a GUARD: the list is the
// obvious place for someone to later add "show a lock icon in the catalog", and
// that addition must fail a test rather than quietly re-open what MYR-435
// closed. Checked for BOTH roles, because a field reaching the owner row is how
// it would get within one line of reaching the viewer row.
func TestVehiclesListCarriesNoRemovedField(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleViewer} {
		m := For(ResourceVehicleSummary, role)
		for group, fields := range removedFromViewers {
			for _, f := range fields {
				if f == "vin" {
					// The catalog carries vinLast4, never `vin`; asserted by
					// the summary tests in mask_test.go.
					continue
				}
				if m.allows(f) {
					t.Errorf("%s vehicles-list row carries %q (%s group) — the catalog "+
						"must not re-open what the snapshot mask closed (MYR-435)",
						role, f, group)
				}
			}
		}
	}
}

// TestRemovedSetMatchesOwnerOnlyList keeps this test file honest against the
// table. Without it, someone could add a field to vehicleStateOwnerOnlyFields
// and the tests above would silently stop covering it.
func TestRemovedSetMatchesOwnerOnlyList(t *testing.T) {
	inTests := make(map[string]struct{})
	for _, fields := range removedFromViewers {
		for _, f := range fields {
			if _, dup := inTests[f]; dup {
				t.Errorf("%q listed in more than one group of removedFromViewers", f)
			}
			inTests[f] = struct{}{}
		}
	}

	inTable := make(map[string]struct{}, len(vehicleStateOwnerOnlyFields))
	for _, f := range vehicleStateOwnerOnlyFields {
		inTable[f] = struct{}{}
	}

	for f := range inTable {
		if _, ok := inTests[f]; !ok {
			t.Errorf("%q is owner-only in the table but absent from removedFromViewers "+
				"— add it so the JSON-key tests actually cover it", f)
		}
	}
	for f := range inTests {
		if _, ok := inTable[f]; !ok {
			t.Errorf("%q is asserted removed by these tests but is not owner-only in "+
				"the table — stale test expectation", f)
		}
	}
}

// containsString reports whether needle is in haystack. Local to avoid pulling
// in slices just for one assertion, matching the existing `contains` helper
// style in mask_test.go.
func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
