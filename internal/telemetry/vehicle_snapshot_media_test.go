package telemetry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/auth"
)

// mediaSnapshotWireKeys is the full MYR-303/308 wire key set. Both roles must
// receive every one of them (see the mask rationale in internal/mask/tables.go).
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

// TestVehicleSnapshotHandler_MediaNowPlayingBothRoles is the MYR-303/308
// handler-level proof. Emitting the five free-text P1 fields to the VIEWER role
// is a deliberate product decision, not an oversight: a rider in the car can
// already hear what is playing, so a now-playing panel that blanks for the
// passenger is the feature failing. This test pins that decision so a later
// "tighten the viewer mask" change has to argue with it explicitly.
func TestVehicleSnapshotHandler_MediaNowPlayingBothRoles(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleViewer} {
		t.Run(string(role)+" receives the full now-playing block", func(t *testing.T) {
			row := fixtureSnapshotRow(mediaFixtureUserID)
			populateMediaRow(&row)

			body := decodeSnapshotBodyForRole(t, row, role)

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
					t.Errorf("%s: key missing from the %s projection", key, role)
					continue
				}
				if got != exp {
					t.Errorf("%s: got %v (%T), want %v (%T)", key, got, got, exp, exp)
				}
			}
		})
	}
}

// TestVehicleSnapshotHandler_MediaNeverSeenIsExplicitNull matches the sibling
// absent-vs-null semantics (MYR-274/298): toMaskMap always writes the key, so a
// never-observed field is an explicit JSON null — never an omitted key and never
// a fabricated value. For seatCoolingCapable that null specifically must not be
// a fabricated `false`, which would tell clients the car has no cooled seats.
func TestVehicleSnapshotHandler_MediaNeverSeenIsExplicitNull(t *testing.T) {
	for _, role := range []auth.Role{auth.RoleOwner, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			// fixtureSnapshotRow leaves all nine nil — the never-observed car.
			body := decodeSnapshotBodyForRole(t, fixtureSnapshotRow(mediaFixtureUserID), role)

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
		})
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
