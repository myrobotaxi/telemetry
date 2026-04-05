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
)

// ErrNoResult is returned when a reverse geocode lookup finds no
// matching place for the given coordinates. Callers should treat this
// as a graceful fallback condition, not a hard failure.
var ErrNoResult = errors.New("geocode: no result for coordinates")

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
	token  string
	client *http.Client
}

// NewMapboxGeocoder creates a MapboxGeocoder with the given API token
// and request timeout. Returns nil if token is empty (geocoding
// disabled).
func NewMapboxGeocoder(token string, timeout time.Duration) *MapboxGeocoder {
	if token == "" {
		return nil
	}
	return &MapboxGeocoder{
		token: token,
		client: &http.Client{
			Timeout: timeout,
		},
	}
}

// ReverseGeocode calls the Mapbox Geocoding API to convert lat/lng into
// a place name and address. Returns ErrNoResult when the API returns no
// matching features. Returns other errors on network or API failures.
func (g *MapboxGeocoder) ReverseGeocode(ctx context.Context, lat, lng float64) (*Result, error) {
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
