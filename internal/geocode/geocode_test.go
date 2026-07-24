package geocode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// queryLat / queryLng are the coordinates used as inputs across the
// table-driven tests below. Other coordinates referenced in tests are
// expressed as offsets from these so the haversine distance is
// predictable: at this latitude, ~0.00009 deg in lng ≈ 10m.
const (
	queryLat = 30.2672
	queryLng = -97.7431
)

func TestMapboxGeocoder_ReverseGeocode(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantResult *Result
		wantErr    error
	}{
		{
			// POI ~11m east of the query coord (well within the 50m
			// threshold) followed by an address feature. PlaceName
			// should be the POI's text; Address should come from the
			// address feature's place_name.
			name:       "POI within threshold",
			statusCode: http.StatusOK,
			body: `{
				"features": [
					{
						"text": "Whole Foods Market",
						"place_name": "Whole Foods Market, 525 N Lamar Blvd, Austin, TX 78703",
						"place_type": ["poi"],
						"center": [-97.7430, 30.2672]
					},
					{
						"text": "525 N Lamar Blvd",
						"place_name": "525 N Lamar Blvd, Austin, TX 78703",
						"place_type": ["address"],
						"center": [-97.7431, 30.2672]
					}
				]
			}`,
			wantResult: &Result{
				PlaceName: "Whole Foods Market",
				Address:   "525 N Lamar Blvd, Austin, TX 78703",
			},
		},
		{
			// POI ~200m north of the query coord (outside threshold);
			// an address feature is at the query coord. PlaceName must
			// be empty so downstream consumers fall back to the street
			// address.
			name:       "POI too far",
			statusCode: http.StatusOK,
			body: `{
				"features": [
					{
						"text": "Far Away Cafe",
						"place_name": "Far Away Cafe, 999 Elsewhere St, Austin, TX",
						"place_type": ["poi"],
						"center": [-97.7431, 30.2690]
					},
					{
						"text": "100 Tributary Way",
						"place_name": "100 Tributary Way, Austin, TX 78703",
						"place_type": ["address"],
						"center": [-97.7431, 30.2672]
					}
				]
			}`,
			wantResult: &Result{
				PlaceName: "",
				Address:   "100 Tributary Way, Austin, TX 78703",
			},
		},
		{
			// No POI features at all (residential drive). PlaceName
			// stays empty; Address comes from the first address
			// feature's place_name.
			name:       "no POI at all",
			statusCode: http.StatusOK,
			body: `{
				"features": [
					{
						"text": "Tributary Way",
						"place_name": "1234 Tributary Way, Austin, TX 78704",
						"place_type": ["address"],
						"center": [-97.7431, 30.2672]
					}
				]
			}`,
			wantResult: &Result{
				PlaceName: "",
				Address:   "1234 Tributary Way, Austin, TX 78704",
			},
		},
		{
			// Multiple POIs in the response: POI "A" is within
			// threshold and appears FIRST in Mapbox's relevance order;
			// POI "B" is also within threshold but listed later. We
			// trust Mapbox's ranking and pick the FIRST qualifying POI
			// (not the geometrically closest), so PlaceName == "A".
			name:       "multiple POIs, first qualifying wins",
			statusCode: http.StatusOK,
			body: `{
				"features": [
					{
						"text": "A",
						"place_name": "A, 100 Main St",
						"place_type": ["poi"],
						"center": [-97.7428, 30.2672]
					},
					{
						"text": "B",
						"place_name": "B, 200 Side St",
						"place_type": ["poi"],
						"center": [-97.74305, 30.2672]
					},
					{
						"text": "100 Main St",
						"place_name": "100 Main St, Austin, TX",
						"place_type": ["address"],
						"center": [-97.7431, 30.2672]
					}
				]
			}`,
			wantResult: &Result{
				PlaceName: "A",
				Address:   "100 Main St, Austin, TX",
			},
		},
		{
			// MYR-206: POI within threshold beats a neighborhood feature
			// — the POI tier is strictly above the neighborhood tier.
			name:       "POI within threshold beats neighborhood",
			statusCode: http.StatusOK,
			body: `{
				"features": [
					{
						"text": "Whole Foods Market",
						"place_name": "Whole Foods Market, 525 N Lamar Blvd, Austin, TX 78703",
						"place_type": ["poi"],
						"center": [-97.7430, 30.2672]
					},
					{
						"text": "525 N Lamar Blvd",
						"place_name": "525 N Lamar Blvd, Austin, TX 78703",
						"place_type": ["address"],
						"center": [-97.7431, 30.2672]
					},
					{
						"text": "Old West Austin",
						"place_name": "Old West Austin, Austin, Texas",
						"place_type": ["neighborhood"],
						"center": [-97.7550, 30.2800]
					}
				]
			}`,
			wantResult: &Result{
				PlaceName: "Whole Foods Market",
				Address:   "525 N Lamar Blvd, Austin, TX 78703",
			},
		},
		{
			// MYR-206: order-independence — the neighborhood feature is
			// listed FIRST in the response array, before a qualifying
			// POI. The tier ranking must still pick the POI: candidates
			// are collected per tier and resolved afterwards, so array
			// order between tiers is irrelevant.
			name:       "neighborhood listed before qualifying POI, POI still wins",
			statusCode: http.StatusOK,
			body: `{
				"features": [
					{
						"text": "Old West Austin",
						"place_name": "Old West Austin, Austin, Texas",
						"place_type": ["neighborhood"],
						"center": [-97.7550, 30.2800]
					},
					{
						"text": "525 N Lamar Blvd",
						"place_name": "525 N Lamar Blvd, Austin, TX 78703",
						"place_type": ["address"],
						"center": [-97.7431, 30.2672]
					},
					{
						"text": "Whole Foods Market",
						"place_name": "Whole Foods Market, 525 N Lamar Blvd, Austin, TX 78703",
						"place_type": ["poi"],
						"center": [-97.7430, 30.2672]
					}
				]
			}`,
			wantResult: &Result{
				PlaceName: "Whole Foods Market",
				Address:   "525 N Lamar Blvd, Austin, TX 78703",
			},
		},
		{
			// MYR-206: the residential case this fallback exists for —
			// no POI within walking range, but Mapbox returns the
			// containing neighborhood. PlaceName uses the neighborhood's
			// text; the neighborhood's centroid distance is irrelevant
			// (containment, not proximity).
			name:       "no POI in range, neighborhood fallback used",
			statusCode: http.StatusOK,
			body: `{
				"features": [
					{
						"text": "Far Away Cafe",
						"place_name": "Far Away Cafe, 999 Elsewhere St, Austin, TX",
						"place_type": ["poi"],
						"center": [-97.7431, 30.2690]
					},
					{
						"text": "100 Tributary Way",
						"place_name": "100 Tributary Way, Austin, TX 78703",
						"place_type": ["address"],
						"center": [-97.7431, 30.2672]
					},
					{
						"text": "Preston Highlands",
						"place_name": "Preston Highlands, Dallas, Texas",
						"place_type": ["neighborhood"],
						"center": [-97.7600, 30.2900]
					}
				]
			}`,
			wantResult: &Result{
				PlaceName: "Preston Highlands",
				Address:   "100 Tributary Way, Austin, TX 78703",
			},
		},
		{
			// MYR-206: no POI and no neighborhood — PlaceName stays
			// empty (clients fall back to the address). Locality-level
			// features must NOT be promoted into PlaceName: a bare city
			// name renders "Dallas → Dallas" on intra-city drives.
			name:       "no POI, no neighborhood, locality never used",
			statusCode: http.StatusOK,
			body: `{
				"features": [
					{
						"text": "100 Tributary Way",
						"place_name": "100 Tributary Way, Austin, TX 78703",
						"place_type": ["address"],
						"center": [-97.7431, 30.2672]
					},
					{
						"text": "Dallas",
						"place_name": "Dallas, Texas, United States",
						"place_type": ["locality"],
						"center": [-96.7970, 32.7767]
					},
					{
						"text": "Dallas",
						"place_name": "Dallas, Texas, United States",
						"place_type": ["place"],
						"center": [-96.7970, 32.7767]
					}
				]
			}`,
			wantResult: &Result{
				PlaceName: "",
				Address:   "100 Tributary Way, Austin, TX 78703",
			},
		},
		{
			// MYR-206: locality present alongside a neighborhood — the
			// neighborhood is used and the locality text is ignored,
			// regardless of array order (locality listed first here).
			name:       "locality listed before neighborhood, neighborhood used",
			statusCode: http.StatusOK,
			body: `{
				"features": [
					{
						"text": "Dallas",
						"place_name": "Dallas, Texas, United States",
						"place_type": ["locality"],
						"center": [-96.7970, 32.7767]
					},
					{
						"text": "Highland Park",
						"place_name": "Highland Park, Dallas, Texas",
						"place_type": ["neighborhood"],
						"center": [-96.8000, 32.8300]
					},
					{
						"text": "100 Tributary Way",
						"place_name": "100 Tributary Way, Austin, TX 78703",
						"place_type": ["address"],
						"center": [-97.7431, 30.2672]
					}
				]
			}`,
			wantResult: &Result{
				PlaceName: "Highland Park",
				Address:   "100 Tributary Way, Austin, TX 78703",
			},
		},
		{
			// MYR-206: a feature dual-tagged ["neighborhood","locality"]
			// is excluded from the neighborhood tier — locality text
			// must never leak into PlaceName under ANY input, so an
			// ambiguous dual-tagged feature loses to the empty floor.
			name:       "dual-tagged neighborhood+locality excluded from PlaceName",
			statusCode: http.StatusOK,
			body: `{
				"features": [
					{
						"text": "Dallas",
						"place_name": "Dallas, Texas, United States",
						"place_type": ["neighborhood", "locality"],
						"center": [-96.7970, 32.7767]
					},
					{
						"text": "100 Tributary Way",
						"place_name": "100 Tributary Way, Austin, TX 78703",
						"place_type": ["address"],
						"center": [-97.7431, 30.2672]
					}
				]
			}`,
			wantResult: &Result{
				PlaceName: "",
				Address:   "100 Tributary Way, Austin, TX 78703",
			},
		},
		{
			// MYR-206: multiple neighborhood features — the first one in
			// Mapbox's relevance order wins within the tier, mirroring
			// the "multiple POIs, first qualifying wins" rule above.
			name:       "multiple neighborhoods, first wins",
			statusCode: http.StatusOK,
			body: `{
				"features": [
					{
						"text": "Preston Highlands",
						"place_name": "Preston Highlands, Dallas, Texas",
						"place_type": ["neighborhood"],
						"center": [-96.8000, 33.0000]
					},
					{
						"text": "Preston Trail",
						"place_name": "Preston Trail, Dallas, Texas",
						"place_type": ["neighborhood"],
						"center": [-96.8100, 33.0100]
					},
					{
						"text": "100 Tributary Way",
						"place_name": "100 Tributary Way, Austin, TX 78703",
						"place_type": ["address"],
						"center": [-97.7431, 30.2672]
					}
				]
			}`,
			wantResult: &Result{
				PlaceName: "Preston Highlands",
				Address:   "100 Tributary Way, Austin, TX 78703",
			},
		},
		{
			// API returns zero features — preserved ErrNoResult
			// behavior so callers (writer_drives, writer_location_address)
			// can treat it as a soft-fail and skip persistence.
			name:       "no features returned",
			statusCode: http.StatusOK,
			body:       `{"features": []}`,
			wantErr:    ErrNoResult,
		},
		{
			name:       "empty features array",
			statusCode: http.StatusOK,
			body:       `{}`,
			wantErr:    ErrNoResult,
		},
		{
			name:       "server error",
			statusCode: http.StatusInternalServerError,
			body:       `{"message": "internal error"}`,
			wantErr:    ErrUpstreamStatus,
		},
		{
			name:       "unauthorized",
			statusCode: http.StatusUnauthorized,
			body:       `{"message": "Not Authorized"}`,
			wantErr:    ErrUpstreamStatus,
		},
		{
			name:       "rate limited (server-side 429)",
			statusCode: http.StatusTooManyRequests,
			body:       `{"message": "Rate limit exceeded"}`,
			wantErr:    ErrRateLimited,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			g := &MapboxGeocoder{
				token:  "test-token",
				client: srv.Client(),
			}
			g.client.Transport = &rewriteTransport{
				base:    srv.Client().Transport,
				baseURL: srv.URL,
			}

			result, err := g.ReverseGeocode(context.Background(), queryLat, queryLng)

			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("expected error matching %v, got: %v", tt.wantErr, err)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.PlaceName != tt.wantResult.PlaceName {
				t.Errorf("PlaceName = %q, want %q", result.PlaceName, tt.wantResult.PlaceName)
			}
			if result.Address != tt.wantResult.Address {
				t.Errorf("Address = %q, want %q", result.Address, tt.wantResult.Address)
			}
		})
	}
}

// TestMapboxGeocoder_AddressFallbackToFirstFeature exercises the
// fallback branch in buildResult: if no feature is tagged "address",
// we use features[0].place_name so the caller always gets something
// rather than an empty string.
func TestMapboxGeocoder_AddressFallbackToFirstFeature(t *testing.T) {
	body := `{
		"features": [
			{
				"text": "Lonely POI",
				"place_name": "Lonely POI, somewhere",
				"place_type": ["poi"],
				"center": [-97.7431, 30.2690]
			}
		]
	}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	g := &MapboxGeocoder{token: "test-token", client: srv.Client()}
	g.client.Transport = &rewriteTransport{base: srv.Client().Transport, baseURL: srv.URL}

	result, err := g.ReverseGeocode(context.Background(), queryLat, queryLng)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// POI is 200m away, so PlaceName is empty.
	if result.PlaceName != "" {
		t.Errorf("PlaceName = %q, want empty", result.PlaceName)
	}
	// No address-typed feature, so Address falls back to features[0].
	if result.Address != "Lonely POI, somewhere" {
		t.Errorf("Address = %q, want fallback to features[0].place_name", result.Address)
	}
}

// TestMapboxGeocoder_RequestFormat is a table-driven regression guard for
// MYR-240: `limit` combined with more than one `types` value makes Mapbox's
// Geocoding v5 API reject the request with HTTP 422 ("limit must be
// combined with a single type parameter when reverse geocoding"), which
// broke 100% of production reverse geocodes. The fix drops `limit`
// entirely, so a 422 from this cause is impossible by construction — this
// test asserts the built request never contains a `limit` param, across
// several real-world coordinates (including the MYR-240 bug report
// coordinate) so the regression can't creep back in for a subset of
// inputs.
func TestMapboxGeocoder_RequestFormat(t *testing.T) {
	tests := []struct {
		name     string
		lat, lng float64
	}{
		{name: "MYR-240 reported failing coordinate", lat: 33.0860, lng: -96.8522},
		{name: "dense downtown-Dallas POI area", lat: 32.7905, lng: -96.8104},
		{name: "Frisco residential street", lat: 33.1301, lng: -96.8236},
		{name: "arbitrary Austin coordinate", lat: 33.0860, lng: -96.8518},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedPath string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				capturedPath = r.URL.String()
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(mapboxResponse{
					Features: []mapboxFeature{
						{
							Text:      "Test",
							PlaceName: "Test Place",
							PlaceType: []string{"address"},
							Center:    [2]float64{tt.lng, tt.lat},
						},
					},
				})
			}))
			defer srv.Close()

			g := &MapboxGeocoder{
				token:  "pk.my-token",
				client: srv.Client(),
			}
			g.client.Transport = &rewriteTransport{
				base:    srv.Client().Transport,
				baseURL: srv.URL,
			}

			_, err := g.ReverseGeocode(context.Background(), tt.lat, tt.lng)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if capturedPath == "" {
				t.Fatal("no request was captured")
			}
			// MYR-240: `limit` must never appear — that's what triggered
			// the 422 when combined with 3 `types` values.
			if strings.Contains(capturedPath, "limit=") {
				t.Errorf("limit param must never be sent (causes 422 with multiple types), got: %s", capturedPath)
			}
			if !strings.Contains(capturedPath, "types=poi,address,neighborhood") {
				t.Errorf("expected types=poi,address,neighborhood in URL, got: %s", capturedPath)
			}
			// Locality/place (city-level) types must never be requested —
			// their text is banned from PlaceName (see buildResult).
			if strings.Contains(capturedPath, "locality") || strings.Contains(capturedPath, "place,") {
				t.Errorf("city-level types must not be requested, got: %s", capturedPath)
			}
			t.Logf("captured path: %s", capturedPath)
		})
	}
}

// TestMapboxGeocoder_MYR240LiveResponseRegression pins the fix against the
// actual Mapbox response shape observed live for the coordinate reported in
// MYR-240 (33.0860, -96.8522) once `limit` was dropped: a 200 OK carrying
// exactly one "address" feature and one "neighborhood" feature (no "poi"
// feature — this Mapbox account/token returns no POI-typed features for
// any of the coordinates checked during the MYR-240 investigation). This
// guards both halves of the fix at once: the request no longer 422s, and
// buildResult's neighborhood-fallback ranking (MYR-206) still produces the
// expected PlaceName/Address pair from that exact response shape.
func TestMapboxGeocoder_MYR240LiveResponseRegression(t *testing.T) {
	body := `{
		"features": [
			{
				"text": "Tributary Way",
				"place_name": "4220 Tributary Way, Frisco, Texas 75034, United States",
				"place_type": ["address"],
				"center": [-96.852371, 33.085983]
			},
			{
				"text": "Stonebriar",
				"place_name": "Stonebriar, Frisco, Texas, United States",
				"place_type": ["neighborhood"],
				"center": [-96.82457, 33.101925]
			}
		]
	}`

	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	g := &MapboxGeocoder{token: "test-token", client: srv.Client()}
	g.client.Transport = &rewriteTransport{base: srv.Client().Transport, baseURL: srv.URL}

	result, err := g.ReverseGeocode(context.Background(), 33.0860, -96.8522)
	if err != nil {
		t.Fatalf("unexpected error (this is the exact MYR-240 422 regression if non-nil): %v", err)
	}
	if strings.Contains(capturedPath, "limit=") {
		t.Fatalf("request must not include limit param, got: %s", capturedPath)
	}
	if result.PlaceName != "Stonebriar" {
		t.Errorf("PlaceName = %q, want %q (neighborhood fallback — no POI in this response)", result.PlaceName, "Stonebriar")
	}
	if result.Address != "4220 Tributary Way, Frisco, Texas 75034, United States" {
		t.Errorf("Address = %q, want %q", result.Address, "4220 Tributary Way, Frisco, Texas 75034, United States")
	}
}

func TestMapboxGeocoder_ContextCancellation(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		// Block until the request context is cancelled.
		<-r.Context().Done()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	g := &MapboxGeocoder{
		token:  "test-token",
		client: srv.Client(),
	}
	g.client.Transport = &rewriteTransport{
		base:    srv.Client().Transport,
		baseURL: srv.URL,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := g.ReverseGeocode(ctx, 30.0, -97.0)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestNewMapboxGeocoder_EmptyToken(t *testing.T) {
	g := NewMapboxGeocoder("", 5*time.Second)
	if g != nil {
		t.Fatal("expected nil for empty token")
	}
}

func TestNewMapboxGeocoder_ValidToken(t *testing.T) {
	g := NewMapboxGeocoder("pk.test123", 5*time.Second)
	if g == nil {
		t.Fatal("expected non-nil geocoder")
	}
}

func TestNoopGeocoder(t *testing.T) {
	g := NoopGeocoder{}
	result, err := g.ReverseGeocode(context.Background(), 30.0, -97.0)
	if !errors.Is(err, ErrNoResult) {
		t.Errorf("expected ErrNoResult, got: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result, got: %v", result)
	}
}

// TestMapboxGeocoder_InvalidCoordinate verifies that input validation
// rejects out-of-range WGS-84 inputs before consuming HTTP / rate-limit
// budget. Maps to MYR-19 acceptance criterion "Tests: invalid coordinates".
func TestMapboxGeocoder_InvalidCoordinate(t *testing.T) {
	tests := []struct {
		name     string
		lat, lng float64
	}{
		{name: "lat > 90", lat: 91, lng: 0},
		{name: "lat < -90", lat: -91, lng: 0},
		{name: "lng > 180", lat: 0, lng: 181},
		{name: "lng < -180", lat: 0, lng: -181},
		{name: "both out of range", lat: 200, lng: 200},
	}

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
	}))
	defer srv.Close()

	g := &MapboxGeocoder{
		token:  "test-token",
		client: srv.Client(),
	}
	g.client.Transport = &rewriteTransport{
		base:    srv.Client().Transport,
		baseURL: srv.URL,
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := g.ReverseGeocode(context.Background(), tt.lat, tt.lng)
			if !errors.Is(err, ErrInvalidCoordinate) {
				t.Errorf("expected ErrInvalidCoordinate, got: %v", err)
			}
		})
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("invalid coords must short-circuit before HTTP; got %d calls", got)
	}
}

// TestMapboxGeocoder_ClientSideRateLimiter verifies that the configured
// rate limiter throttles outbound requests. Maps to MYR-19 acceptance
// criterion "Rate limiting per Mapbox plan".
func TestMapboxGeocoder_ClientSideRateLimiter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"features":[{"text":"X","place_name":"X","place_type":["address"],"center":[-97,30]}]}`))
	}))
	defer srv.Close()

	// 1 request/second, burst 1 — second call must wait ~1s.
	g := &MapboxGeocoder{
		token:   "test-token",
		client:  srv.Client(),
		limiter: rate.NewLimiter(1, 1),
	}
	g.client.Transport = &rewriteTransport{
		base:    srv.Client().Transport,
		baseURL: srv.URL,
	}

	ctx := context.Background()
	if _, err := g.ReverseGeocode(ctx, 30, -97); err != nil {
		t.Fatalf("first call: unexpected error: %v", err)
	}
	start := time.Now()
	if _, err := g.ReverseGeocode(ctx, 30, -97); err != nil {
		t.Fatalf("second call: unexpected error: %v", err)
	}
	elapsed := time.Since(start)
	// Allow 200ms slack for scheduler/CI variance; the second call is
	// blocked at least until the next token is available.
	if elapsed < 600*time.Millisecond {
		t.Errorf("second call returned in %v; expected >= 600ms (rate limiter not enforcing)", elapsed)
	}
}

// TestMapboxGeocoder_RateLimiterCancelledContext verifies that when the
// caller's context is cancelled while waiting for a rate-limit token,
// ReverseGeocode returns promptly with a context error rather than
// burning the token budget on a request that nobody is waiting for.
func TestMapboxGeocoder_RateLimiterCancelledContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"features":[{"text":"X","place_name":"X","place_type":["address"],"center":[-97,30]}]}`))
	}))
	defer srv.Close()

	// burst=0 so every call has to wait — context cancellation hits
	// before any token is available.
	g := &MapboxGeocoder{
		token:   "test-token",
		client:  srv.Client(),
		limiter: rate.NewLimiter(rate.Every(time.Hour), 0),
	}
	g.client.Transport = &rewriteTransport{
		base:    srv.Client().Transport,
		baseURL: srv.URL,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := g.ReverseGeocode(ctx, 30, -97)
	if err == nil {
		t.Fatal("expected error from cancelled context inside rate limiter wait")
	}
}

// TestNewMapboxGeocoderWithLimiter_NilLimiterDisablesThrottle verifies
// that passing a nil limiter to NewMapboxGeocoderWithLimiter yields a
// geocoder that does not throttle at all (used by tests that don't want
// to wait on the default 10 RPS budget).
func TestNewMapboxGeocoderWithLimiter_NilLimiterDisablesThrottle(t *testing.T) {
	g := NewMapboxGeocoderWithLimiter("pk.test", time.Second, nil)
	if g == nil {
		t.Fatal("expected non-nil geocoder")
	}
	if g.limiter != nil {
		t.Errorf("expected nil limiter, got %v", g.limiter)
	}
}

// rewriteTransport intercepts HTTP requests and redirects them to the
// test server, preserving the path and query string.
type rewriteTransport struct {
	base    http.RoundTripper
	baseURL string
}

func (t *rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// Replace the Mapbox API host with the test server.
	r.URL.Scheme = "http"
	r.URL.Host = t.baseURL[len("http://"):]
	return t.base.RoundTrip(r)
}
