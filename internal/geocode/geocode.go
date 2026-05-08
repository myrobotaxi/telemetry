// Package geocode provides reverse geocoding of coordinates into
// human-readable addresses. The primary implementation uses the Mapbox
// Geocoding API, matching the same service used by the MyRoboTaxi
// Next.js frontend.
package geocode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"golang.org/x/time/rate"
)

// ErrNoResult is returned when a reverse geocode lookup finds no
// matching place for the given coordinates. Callers should treat this
// as a graceful fallback condition, not a hard failure.
var ErrNoResult = errors.New("geocode: no result for coordinates")

// ErrInvalidCoordinate is returned when ReverseGeocode is called with
// coordinates outside the valid WGS-84 range (lat in [-90, 90], lng in
// [-180, 180]). Callers should treat this as a permanent failure for
// the given input — retrying with the same coordinates will not help.
var ErrInvalidCoordinate = errors.New("geocode: invalid coordinate")

// defaultRateLimit is the steady-state requests-per-second cap applied
// when NewMapboxGeocoder is used without an explicit limiter. Mapbox's
// free tier permits 600 requests/minute (10 req/s); paid tiers go
// higher. v1 reverse-geocode load is bounded by drive-completion
// frequency (a handful of calls per drive), so the default is well
// below quota for typical fleets.
const defaultRateLimit = rate.Limit(10)

// defaultRateBurst is the burst size applied when NewMapboxGeocoder is
// used without an explicit limiter. A modest burst absorbs transient
// spikes from concurrent drive completions across multiple vehicles
// without exceeding the steady-state cap.
const defaultRateBurst = 10

// Result holds the output of a reverse geocode lookup.
type Result struct {
	// PlaceName is the short place name (e.g. "Thompson Hotel").
	PlaceName string
	// Address is the full street address
	// (e.g. "506 San Jacinto Blvd, Austin, TX 78701").
	Address string
}

// Geocoder reverse geocodes a coordinate pair into a human-readable
// address. Implementations must be safe for concurrent use.
type Geocoder interface {
	ReverseGeocode(ctx context.Context, lat, lng float64) (*Result, error)
}

// mapboxResponse is the minimal subset of the Mapbox Geocoding API
// response that we parse.
type mapboxResponse struct {
	Features []struct {
		Text      string `json:"text"`
		PlaceName string `json:"place_name"`
	} `json:"features"`
}

// MapboxGeocoder calls the Mapbox Geocoding API for reverse geocoding.
type MapboxGeocoder struct {
	token   string
	client  *http.Client
	limiter *rate.Limiter
}

// NewMapboxGeocoder creates a MapboxGeocoder with the given API token
// and request timeout, using the default Mapbox free-tier rate limit
// (10 req/s, burst 10). Returns nil if token is empty (geocoding
// disabled). For non-default rate limits, use
// NewMapboxGeocoderWithLimiter.
func NewMapboxGeocoder(token string, timeout time.Duration) *MapboxGeocoder {
	return NewMapboxGeocoderWithLimiter(
		token, timeout, rate.NewLimiter(defaultRateLimit, defaultRateBurst),
	)
}

// NewMapboxGeocoderWithLimiter creates a MapboxGeocoder with an explicit
// rate limiter so callers can tune the requests-per-second budget to
// their Mapbox plan. Returns nil if token is empty. A nil limiter
// disables client-side throttling and relies on Mapbox's server-side
// 429 response — preferred only for tests.
func NewMapboxGeocoderWithLimiter(token string, timeout time.Duration, limiter *rate.Limiter) *MapboxGeocoder {
	if token == "" {
		return nil
	}
	return &MapboxGeocoder{
		token: token,
		client: &http.Client{
			Timeout: timeout,
		},
		limiter: limiter,
	}
}

// ReverseGeocode calls the Mapbox Geocoding API to convert lat/lng into
// a place name and address. Returns ErrInvalidCoordinate when the input
// is outside the WGS-84 range. Returns ErrNoResult when the API returns
// no matching features. Returns other errors on network or API failures.
// Honors the configured rate limiter — a request that exceeds the
// budget blocks until a token is available or ctx is cancelled.
func (g *MapboxGeocoder) ReverseGeocode(ctx context.Context, lat, lng float64) (*Result, error) {
	if !validCoordinate(lat, lng) {
		return nil, fmt.Errorf("geocode.ReverseGeocode(%.4f,%.4f): %w", lat, lng, ErrInvalidCoordinate)
	}

	if g.limiter != nil {
		if err := g.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("geocode.ReverseGeocode: rate limit wait: %w", err)
		}
	}

	url := fmt.Sprintf(
		"https://api.mapbox.com/geocoding/v5/mapbox.places/%f,%f.json?access_token=%s&limit=1&types=poi,address",
		lng, lat, g.token,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("geocode.ReverseGeocode: build request: %w", err)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("geocode.ReverseGeocode(%.4f,%.4f): %w", lat, lng, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return nil, fmt.Errorf("geocode.ReverseGeocode(%.4f,%.4f): HTTP %d: %s", lat, lng, resp.StatusCode, body)
	}

	var data mapboxResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("geocode.ReverseGeocode: decode: %w", err)
	}

	if len(data.Features) == 0 {
		return nil, ErrNoResult
	}

	return &Result{
		PlaceName: data.Features[0].Text,
		Address:   data.Features[0].PlaceName,
	}, nil
}

// NoopGeocoder always returns ErrNoResult, effectively disabling
// geocoding. Used when no Mapbox token is configured.
type NoopGeocoder struct{}

// ReverseGeocode always returns ErrNoResult.
func (NoopGeocoder) ReverseGeocode(_ context.Context, _, _ float64) (*Result, error) {
	return nil, ErrNoResult
}

// validCoordinate reports whether (lat, lng) is inside the WGS-84 range.
// Out-of-range inputs are rejected before the HTTP call to avoid burning
// rate-limit budget on requests Mapbox would reject anyway.
func validCoordinate(lat, lng float64) bool {
	return lat >= -90 && lat <= 90 && lng >= -180 && lng <= 180
}
