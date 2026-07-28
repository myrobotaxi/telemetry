package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFleetAPIClient_GetReleaseNotes_Decode covers the decode + extraction
// contract. The properties that matter:
//
//   - the FIRST entry wins (Tesla orders newest first), so a car with a history
//     of releases reports its CURRENT one, not its oldest;
//   - the title is passed through VERBATIM — the shape is Tesla's, and a
//     consumer that reformatted or version-compared it would break the moment
//     Tesla changed a parenthetical or a point-release depth;
//   - an EMPTY list is a legitimate non-error answer, distinct from a failure.
func TestFleetAPIClient_GetReleaseNotes_Decode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		body      string
		wantCount int
		wantTitle string // "" means newestReleaseTitle must yield "" (nothing to write)
	}{
		{
			// The live-verified shape from the client's own car.
			name: "newest entry's title is taken verbatim",
			body: `{"response":{"release_notes":[` +
				`{"title":"FSD (Supervised) v14.3.5","subtitle":"What's new","description":"Lots."},` +
				`{"title":"FSD (Supervised) v14.2.0","subtitle":"Older","description":"Also lots."}]}}`,
			wantCount: 2,
			wantTitle: "FSD (Supervised) v14.3.5",
		},
		{
			name:      "single entry",
			body:      `{"response":{"release_notes":[{"title":"FSD (Supervised) v14.3.5"}]}}`,
			wantCount: 1,
			wantTitle: "FSD (Supervised) v14.3.5",
		},
		{
			// A car Tesla has no notes for. NOT an error, and NOT a claim the
			// car lacks FSD — the caller must simply write nothing.
			name:      "empty list is a non-error answer",
			body:      `{"response":{"release_notes":[]}}`,
			wantCount: 0,
		},
		{
			name:      "absent release_notes key",
			body:      `{"response":{}}`,
			wantCount: 0,
		},
		{
			name:      "absent response object entirely",
			body:      `{}`,
			wantCount: 0,
		},
		{
			// An entry with no title is indistinguishable, for our purposes,
			// from no entry: both mean "nothing to write".
			name:      "newest entry has no title",
			body:      `{"response":{"release_notes":[{"subtitle":"no title here"}]}}`,
			wantCount: 1,
		},
		{
			name:      "newest entry has an empty title",
			body:      `{"response":{"release_notes":[{"title":""},{"title":"FSD (Supervised) v14.2.0"}]}}`,
			wantCount: 2,
			// Deliberately NOT falling through to the older entry: an empty
			// newest title means Tesla told us nothing about the CURRENT
			// release, and reporting a superseded one would be worse than
			// reporting nothing.
		},
		{
			// Whatever Tesla sends is what we store. No trimming, no case
			// folding, no `v`-prefix stripping.
			name:      "unusual shape is preserved unchanged",
			body:      `{"response":{"release_notes":[{"title":"  FSD Beta 99 (Experimental)  "}]}}`,
			wantCount: 1,
			wantTitle: "  FSD Beta 99 (Experimental)  ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gotPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)

			client := NewFleetAPIClient(FleetAPIConfig{BaseURL: srv.URL}, fleetTestLogger())
			notes, err := client.GetReleaseNotes(context.Background(), "token", testVIN)
			if err != nil {
				t.Fatalf("GetReleaseNotes: %v", err)
			}

			if want := "/api/1/vehicles/" + testVIN + "/release_notes"; gotPath != want {
				t.Errorf("path = %q, want %q", gotPath, want)
			}
			if len(notes) != tt.wantCount {
				t.Fatalf("len(notes) = %d, want %d", len(notes), tt.wantCount)
			}
			if got := newestReleaseTitle(notes); got != tt.wantTitle {
				t.Errorf("newestReleaseTitle = %q, want %q", got, tt.wantTitle)
			}
		})
	}
}

// A malformed body must fail cleanly — and, because release prose is unbounded
// free text, the error must not carry the body.
func TestFleetAPIClient_GetReleaseNotes_Malformed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{name: "not JSON at all", body: `<html>502 Bad Gateway</html>`},
		{name: "truncated JSON", body: `{"response":{"release_notes":[{"title":"FSD`},
		{name: "release_notes is not an array", body: `{"response":{"release_notes":"nope"}}`},
		{name: "entry is not an object", body: `{"response":{"release_notes":["nope"]}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tt.body))
			}))
			t.Cleanup(srv.Close)

			client := NewFleetAPIClient(FleetAPIConfig{BaseURL: srv.URL}, fleetTestLogger())
			_, err := client.GetReleaseNotes(context.Background(), "token", testVIN)
			if err == nil {
				t.Fatal("expected a decode error")
			}
			if strings.Contains(err.Error(), tt.body) {
				t.Errorf("error leaked the response body: %v", err)
			}
		})
	}
}

// newestReleaseTitle must survive a nil slice — the shape a failed read leaves
// behind — without panicking.
func TestNewestReleaseTitleNilSafe(t *testing.T) {
	t.Parallel()
	if got := newestReleaseTitle(nil); got != "" {
		t.Errorf("newestReleaseTitle(nil) = %q, want \"\"", got)
	}
}

// The guards must reject before any network call, and must never put a raw VIN
// in the error string.
func TestFleetAPIClient_GetReleaseNotes_InputGuards(t *testing.T) {
	t.Parallel()

	client := NewFleetAPIClient(FleetAPIConfig{BaseURL: "http://127.0.0.1:1"}, fleetTestLogger())

	t.Run("empty token", func(t *testing.T) {
		t.Parallel()
		if _, err := client.GetReleaseNotes(context.Background(), "", testVIN); err == nil {
			t.Fatal("expected an error for an empty token")
		}
	})

	t.Run("short VIN is rejected and redacted", func(t *testing.T) {
		t.Parallel()
		const badVIN = "TOOSHORT"
		_, err := client.GetReleaseNotes(context.Background(), "token", badVIN)
		if err == nil {
			t.Fatal("expected an error for an invalid VIN")
		}
		if strings.Contains(err.Error(), badVIN) {
			t.Errorf("error leaked the raw VIN: %v", err)
		}
	})
}

// A non-2xx surfaces as a *FleetAPIError so callers can classify it — notably
// the 408 that means "the car is asleep", which is the ORDINARY outcome for a
// parked or in-service vehicle and must never be escalated.
func TestFleetAPIClient_GetReleaseNotes_UpstreamError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestTimeout)
		_, _ = w.Write([]byte(`{"error":"vehicle unavailable"}`))
	}))
	t.Cleanup(srv.Close)

	client := NewFleetAPIClient(FleetAPIConfig{BaseURL: srv.URL}, fleetTestLogger())
	if _, err := client.GetReleaseNotes(context.Background(), "token", testVIN); err == nil {
		t.Fatal("expected an error for a 408 response")
	} else if !isFleetStatus(err, http.StatusRequestTimeout) {
		t.Fatalf("error did not carry the Fleet 408: %v", err)
	}
}
