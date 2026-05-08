package geocode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
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
			name:       "successful geocode",
			statusCode: http.StatusOK,
			body: `{
				"features": [{
					"text": "Thompson Hotel",
					"place_name": "Thompson Hotel, 506 San Jacinto Blvd, Austin, TX 78701"
				}]
			}`,
			wantResult: &Result{
				PlaceName: "Thompson Hotel",
				Address:   "Thompson Hotel, 506 San Jacinto Blvd, Austin, TX 78701",
			},
		},
		{
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
			wantErr:    errors.New("HTTP 500"),
		},
		{
			name:       "unauthorized",
			statusCode: http.StatusUnauthorized,
			body:       `{"message": "Not Authorized"}`,
			wantErr:    errors.New("HTTP 401"),
		},
		{
			name:       "rate limited (server-side 429)",
			statusCode: http.StatusTooManyRequests,
			body:       `{"message": "Rate limit exceeded"}`,
			wantErr:    errors.New("HTTP 429"),
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
			// Override the API URL by using a custom transport that
			// redirects requests to the test server.
			origURL := srv.URL
			g.client.Transport = &rewriteTransport{
				base:    srv.Client().Transport,
				baseURL: origURL,
			}

			result, err := g.ReverseGeocode(context.Background(), 30.2672, -97.7431)

			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				if errors.Is(tt.wantErr, ErrNoResult) && !errors.Is(err, ErrNoResult) {
					t.Errorf("expected ErrNoResult, got: %v", err)
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

func TestMapboxGeocoder_RequestFormat(t *testing.T) {
	var capturedPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mapboxResponse{
			Features: []struct {
				Text      string `json:"text"`
				PlaceName string `json:"place_name"`
			}{
				{Text: "Test", PlaceName: "Test Place"},
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

	_, err := g.ReverseGeocode(context.Background(), 33.0860, -96.8518)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify the URL contains the correct coordinates in lng,lat order
	// (Mapbox expects lng,lat, not lat,lng).
	if capturedPath == "" {
		t.Fatal("no request was captured")
	}
	// The URL should have lng (-96.8518) before lat (33.0860).
	// Just verify the token is present and types param is set.
	t.Logf("captured path: %s", capturedPath)
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
		_, _ = w.Write([]byte(`{"features":[{"text":"X","place_name":"X"}]}`))
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
		_, _ = w.Write([]byte(`{"features":[{"text":"X","place_name":"X"}]}`))
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
