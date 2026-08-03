package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/auth"
)

// mediaSnapshotWireKeys is the full MYR-303/308 wire key set.
//
// MYR-435 (client decision, 2026-08-02) made these OWNER-ONLY. They used to be
// a both-roles set; the rationale for the reversal lives in
// internal/mask/tables.go (vehicleStateOwnerOnlyFields). Every assertion in this
// file that used to loop over both roles is now split: owner keeps the block,
// viewer must not receive a single one of these keys.
var mediaSnapshotWireKeys = []string{
	"mediaNowPlayingTitle",
	"mediaNowPlayingArtist",
	"mediaNowPlayingAlbum",
	"mediaNowPlayingStation",
	"mediaPlaybackSource",
	"mediaNowPlayingDurationMs",
	"mediaNowPlayingElapsedMs",
	"mediaVolumeMax",
	"seatCoolingCapable",
}

// mediaFixtureUserID is the owner of every fixture row in this file. Pinned as a
// const rather than threaded through decodeSnapshotBodyForRole as a parameter,
// which the unparam linter (correctly) flags as always-constant — same reasoning
// as fixtureSnapshotRowID.
const mediaFixtureUserID = "user-1"

// decodeSnapshotBodyForRole runs the snapshot handler for one role and returns
// the decoded JSON body.
func decodeSnapshotBodyForRole(t *testing.T, row VehicleSnapshotRow, role auth.Role) map[string]any {
	t.Helper()
	h := NewVehicleSnapshotHandler(
		&stubTokenValidator{userID: mediaFixtureUserID},
		&stubVehicleSnapshotReader{row: row},
		discardLogger(),
		WithSnapshotRoleResolver(&stubRoleResolver{role: role}),
	)
	mux := http.NewServeMux()
	mux.Handle("GET /api/vehicles/{vehicleId}/snapshot", h)

	req := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"/api/vehicles/"+fixtureSnapshotRowID+"/snapshot",
		nil,
	)
	req.Header.Set("Authorization", "Bearer t")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return body
}

// populateMediaRow fills the MYR-303/308 fields on a snapshot row.
func populateMediaRow(row *VehicleSnapshotRow) {
	title := "Summertime Friends"
	artist := "The Chainsmokers"
	album := "Summertime Friends"
	station := "Alt Nation"
	source := "Spotify"
	duration := int64(214000)
	elapsed := int64(42000)
	volMax := 11.0
	capable := true

	row.MediaNowPlayingTitle = &title
	row.MediaNowPlayingArtist = &artist
	row.MediaNowPlayingAlbum = &album
	row.MediaNowPlayingStation = &station
	row.MediaPlaybackSource = &source
	row.MediaNowPlayingDuration = &duration
	row.MediaNowPlayingElapsed = &elapsed
	row.MediaVolumeMax = &volMax
	row.SeatCoolingCapable = &capable
}

// TestVehicleSnapshotHandler_MediaNowPlayingOwnerOnly is the MYR-303/308
// handler-level proof, rewritten for the MYR-435 client decision.
//
// It used to assert that BOTH roles received the five free-text P1 fields, on
// the reasoning that a rider in the car can already hear what is playing. The
// client reversed that on 2026-08-02 ("Remove media/cabin and any vehicle
// controls"), and the reasoning that had to give way is worth keeping here: a
// share grant is durable and REMOTE, so a viewer is frequently not in the car
// at all — the "they can already hear it" defense only ever held for the
// minutes someone was in the passenger seat, and the mask cannot tell.
//
// This is the REST snapshot half of the both-surfaces check; the WS half is in
// internal/ws, and both consult the same table (internal/mask/tables.go).
func TestVehicleSnapshotHandler_MediaNowPlayingOwnerOnly(t *testing.T) {
	t.Run("owner receives the full now-playing block", func(t *testing.T) {
		row := fixtureSnapshotRow(mediaFixtureUserID)
		populateMediaRow(&row)

		body := decodeSnapshotBodyForRole(t, row, auth.RoleOwner)

		want := map[string]any{
			"mediaNowPlayingTitle":      "Summertime Friends",
			"mediaNowPlayingArtist":     "The Chainsmokers",
			"mediaNowPlayingAlbum":      "Summertime Friends",
			"mediaNowPlayingStation":    "Alt Nation",
			"mediaPlaybackSource":       "Spotify",
			"mediaNowPlayingDurationMs": float64(214000),
			"mediaNowPlayingElapsedMs":  float64(42000),
			"mediaVolumeMax":            float64(11),
			"seatCoolingCapable":        true,
		}
		for key, exp := range want {
			got, ok := body[key]
			if !ok {
				t.Errorf("%s: key missing from the owner projection — MYR-435 narrowed "+
					"the VIEWER arm only", key)
				continue
			}
			if got != exp {
				t.Errorf("%s: got %v (%T), want %v (%T)", key, got, got, exp, exp)
			}
		}
	})

	t.Run("viewer receives no media key at all", func(t *testing.T) {
		row := fixtureSnapshotRow(mediaFixtureUserID)
		populateMediaRow(&row)

		body := decodeSnapshotBodyForRole(t, row, auth.RoleViewer)

		// Absent, not nulled (rest-api.md §5.1): the KEY must be gone. A null
		// would still tell the viewer the field exists.
		for _, key := range mediaSnapshotWireKeys {
			if got, present := body[key]; present {
				t.Errorf("%s: present in the viewer snapshot (value %v) — MYR-435 removes "+
					"media and cabin state from viewers entirely", key, got)
			}
		}

		// The values themselves must not survive under any key. Re-encode the
		// decoded body and search the bytes, so a rename or a nesting change
		// could not hide the leak from the key check above.
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("re-marshal viewer body: %v", err)
		}
		for _, secret := range []string{"The Chainsmokers", "Alt Nation", "Spotify", "Summertime Friends"} {
			if bytes.Contains(encoded, []byte(secret)) {
				t.Errorf("viewer snapshot leaks media content %q: %s", secret, encoded)
			}
		}

		// Sanity: the viewer still got a usable snapshot. A mask bug that
		// emptied the whole body would otherwise pass every assertion above.
		if _, ok := body["chargeLevel"]; !ok {
			t.Error("viewer snapshot has no chargeLevel — the narrowing removed too much")
		}
	})
}

// TestVehicleSnapshotHandler_MediaNeverSeenIsExplicitNull matches the sibling
// absent-vs-null semantics (MYR-274/298): toMaskMap always writes the key, so a
// never-observed field is an explicit JSON null — never an omitted key and never
// a fabricated value. For seatCoolingCapable that null specifically must not be
// a fabricated `false`, which would tell clients the car has no cooled seats.
//
// OWNER ONLY since MYR-435. For a viewer the distinction does not arise: the key
// is absent whether or not the car ever reported it, which is the point — a
// viewer cannot tell a silent radio from a masked one, and should not be able to.
func TestVehicleSnapshotHandler_MediaNeverSeenIsExplicitNull(t *testing.T) {
	// fixtureSnapshotRow leaves all nine nil — the never-observed car.
	body := decodeSnapshotBodyForRole(t, fixtureSnapshotRow(mediaFixtureUserID), auth.RoleOwner)

	for _, key := range mediaSnapshotWireKeys {
		got, ok := body[key]
		if !ok {
			t.Errorf("%s: key missing; siblings keep the key with a null value", key)
			continue
		}
		if got != nil {
			t.Errorf("%s: got %v, want explicit null (honest-unknown)", key, got)
		}
	}
}

// TestVehicleSnapshotHandler_MediaEmptyStringIsNotNull is the wire-level half of
// the MYR-303 empty-vs-null decision. A cleared track must serialize as "" and
// MUST NOT be collapsed into null: null means "we have never heard", "" means
// "we know, and nothing is playing", and a client renders those differently.
func TestVehicleSnapshotHandler_MediaEmptyStringIsNotNull(t *testing.T) {
	row := fixtureSnapshotRow(mediaFixtureUserID)
	empty := ""
	row.MediaNowPlayingTitle = &empty
	row.MediaNowPlayingArtist = &empty
	// Artist cleared, album never observed — the two must not look alike.

	body := decodeSnapshotBodyForRole(t, row, auth.RoleOwner)

	for _, key := range []string{"mediaNowPlayingTitle", "mediaNowPlayingArtist"} {
		got, ok := body[key]
		if !ok {
			t.Fatalf("%s: key missing", key)
		}
		if got == nil {
			t.Errorf("%s: got null, want \"\" — a cleared track must stay distinguishable "+
				"from never-observed", key)
			continue
		}
		if got != "" {
			t.Errorf("%s: got %v, want \"\"", key, got)
		}
	}

	if got, ok := body["mediaNowPlayingAlbum"]; !ok || got != nil {
		t.Errorf("mediaNowPlayingAlbum: got %v (present=%t), want explicit null (never observed)", got, ok)
	}
}

// TestSnapshotMediaMaskKeysArePresentInResponse guards the mask/response pair.
// A field added to the response struct but forgotten in the mask allow-list is
// silently stripped, which is invisible in a struct-level test — so assert every
// key survives an actual role projection.
func TestSnapshotMediaMaskKeysArePresentInResponse(t *testing.T) {
	row := fixtureSnapshotRow(mediaFixtureUserID)
	populateMediaRow(&row)
	body := decodeSnapshotBodyForRole(t, row, auth.RoleOwner)

	for _, key := range mediaSnapshotWireKeys {
		if _, ok := body[key]; !ok {
			t.Errorf("%s: stripped by the owner mask — add it to vehicleStateOwnerFields", key)
		}
	}
}
