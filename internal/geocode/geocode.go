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

// ErrRateLimited is returned when the Mapbox API responds with HTTP 429
// (Too Many Requests). Callers should back off before retrying rather
// than treating this the same as ErrUpstreamStatus.
var ErrRateLimited = errors.New("geocode: rate limited by upstream")

// ErrUpstreamStatus is returned when the Mapbox API responds with a
// non-200, non-429 status code. The numeric status code is safe to
// include in the wrapping error text (see ReverseGeocode) — it carries
// no coordinates, tokens, or response body.
var ErrUpstreamStatus = errors.New("geocode: upstream returned an error status")

// ErrTransport is returned for network-level failures reaching the
// Mapbox API: request construction, DNS, connection, TLS, timeout, or
// response-decode failures. Deliberately opaque (MYR-254): the
// underlying errors here — in particular the *url.Error returned by
// (*http.Client).Do — embed the full request URL, which carries the
// full-precision queried coordinates AND the access_token secret as
// query parameters. Never unwrap or format the underlying error into
// this one; classify only.
var ErrTransport = errors.New("geocode: transport error calling upstream")

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

// poiDistanceThreshold is the maximum distance, in meters, between the
// queried coordinate and a POI feature's center for that POI to be
// treated as the place name. Roughly a one-minute walk — close enough
// that the driver is plausibly *at* the POI, far enough to absorb GPS
// jitter and Mapbox's own POI centroid placement.
//
// Tuned for residential drives: Mapbox often returns a street-name
// "address" feature as the closest result, which makes a poor label
// ("Tributary Way"). Falling back to an empty PlaceName lets downstream
// consumers render the full street address instead.
const poiDistanceThreshold = 50.0

// Result holds the output of a reverse geocode lookup.
type Result struct {
	// PlaceName is the short place name. A POI within
	// poiDistanceThreshold wins (e.g. "Thompson Hotel"); otherwise the
	// containing neighborhood's name is used (e.g. "Preston Highlands")
	// so residential drive endpoints still get a human-readable label.
	// Empty when neither is available — consumers fall back to Address.
	//
	// PlaceName is NEVER a bare city/locality name: an intra-city drive
	// would render "Dallas → Dallas", which client QA explicitly
	// rejected (MYR-206). Neighborhood is the floor of the fallback
	// chain.
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
// response that we parse. Feature ordering, place_type, and center are
// all load-bearing — see ReverseGeocode for how features are ranked.
type mapboxResponse struct {
	Features []mapboxFeature `json:"features"`
}

// mapboxFeature is a single feature in a Mapbox Geocoding v5 response.
// PlaceType is a list because Mapbox can tag a feature with multiple
// types (e.g. ["poi", "landmark"]). Center is [lng, lat] per the v5
// GeoJSON convention — NOT [lat, lng].
type mapboxFeature struct {
	Text      string     `json:"text"`
	PlaceName string     `json:"place_name"`
	PlaceType []string   `json:"place_type"`
	Center    [2]float64 `json:"center"`
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
// no matching features. Returns ErrRateLimited, ErrUpstreamStatus, or
// ErrTransport on network/API failures — see those sentinels' docs.
// Honors the configured rate limiter — a request that exceeds the
// budget blocks until a token is available or ctx is cancelled.
//
// MYR-254: every error this method returns is guaranteed free of the
// queried coordinates, the request URL, the access_token secret, and
// the upstream response body — all of which are P1 data
// (docs/contracts/data-classification.md §2.2) that callers routinely
// log verbatim via err.Error(). Do not reintroduce any of the four into
// an error string here without updating that guarantee (and the tests
// that pin it) at the same time.
func (g *MapboxGeocoder) ReverseGeocode(ctx context.Context, lat, lng float64) (*Result, error) {
	if !validCoordinate(lat, lng) {
		return nil, fmt.Errorf("geocode.ReverseGeocode: %w", ErrInvalidCoordinate)
	}

	if g.limiter != nil {
		if err := g.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("geocode.ReverseGeocode: rate limit wait: %w", err)
		}
	}

	// MYR-240: NO `limit` param here. Mapbox's Geocoding v5 API rejects
	// `limit` combined with more than one `types` value on a reverse
	// lookup with HTTP 422 ("limit must be combined with a single type
	// parameter when reverse geocoding") — the previous `limit=5` here
	// broke every production reverse geocode outright (Drive.startAddress
	// / endAddress never populated, "Location Unavailable" everywhere).
	//
	// Omitting `limit` is safe, not just legal: per Mapbox's documented
	// reverse-geocode default, a request naming N types returns AT MOST
	// one feature per requested type, so this single call already gives
	// buildResult exactly the poi/address/neighborhood candidates it
	// ranks over, with a response size bounded by len(types) instead of
	// an arbitrary limit. Verified live against api.mapbox.com for
	// MYR-240 across three coordinates (the reported failing coordinate,
	// a dense downtown-Dallas POI area, and a Frisco residential street):
	// all returned HTTP 200 with <= 1 feature per type, never a 422. A
	// two-call split (types=poi&limit=5, then types=address,neighborhood,
	// merged before ranking) was also verified live and returns the same
	// candidates for this Mapbox account, but at double the rate-limiter
	// cost per lookup for no extra ranking benefit — so the single
	// no-limit call is preferred.
	//
	// types includes "neighborhood" (MYR-206) so buildResult can fall
	// back to the containing neighborhood's name when no POI is within
	// walking range — the common residential-endpoint case. "locality"
	// (and coarser city-level types) are deliberately NOT requested:
	// buildResult would only ever discard them (a bare city name in
	// PlaceName renders "Dallas → Dallas" on intra-city drives, rejected
	// by client QA), so requesting them would only waste a type slot in
	// the response.
	url := fmt.Sprintf(
		"https://api.mapbox.com/geocoding/v5/mapbox.places/%f,%f.json?access_token=%s&types=poi,address,neighborhood",
		lng, lat, g.token,
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		// MYR-254: never wrap err here. http.NewRequestWithContext's error
		// embeds the URL it failed to parse — which carries the
		// full-precision coordinates and the access_token secret.
		// Classify only.
		return nil, fmt.Errorf("geocode.ReverseGeocode: build request: %w", ErrTransport)
	}

	resp, err := g.client.Do(req)
	if err != nil {
		// MYR-254: client.Do's error is a *url.Error whose Error() embeds
		// the full request URL — access_token and full-precision
		// coordinates included. Never wrap it; classify only.
		return nil, fmt.Errorf("geocode.ReverseGeocode: %w", ErrTransport)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// MYR-254: drain (don't parse) up to 256B so the connection can be
		// reused, but the body is never placed in the returned error —
		// Mapbox error bodies can echo back query parameters, including
		// the access_token.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 256))
		if resp.StatusCode == http.StatusTooManyRequests {
			return nil, fmt.Errorf("geocode.ReverseGeocode: %w", ErrRateLimited)
		}
		// The numeric status code alone is safe to log/return.
		return nil, fmt.Errorf("geocode.ReverseGeocode: status %d: %w", resp.StatusCode, ErrUpstreamStatus)
	}

	var data mapboxResponse
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("geocode.ReverseGeocode: decode response: %w", ErrTransport)
	}

	if len(data.Features) == 0 {
		return nil, ErrNoResult
	}

	return buildResult(lat, lng, data.Features), nil
}

// ClassifyError reduces any error returned by ReverseGeocode to a coarse,
// log-safe classification string. MYR-254: every error ReverseGeocode
// returns is already free of coordinates, the request URL/access_token,
// and the upstream response body, so err.Error() itself is safe to log —
// ClassifyError exists for callers that want a single structured log
// field (e.g. slog.String("error_class", ...)) instead of duplicating
// errors.Is checks against every sentinel at each call site.
func ClassifyError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrInvalidCoordinate):
		return "invalid_coordinate"
	case errors.Is(err, ErrNoResult):
		return "no_result"
	case errors.Is(err, ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, ErrUpstreamStatus):
		return "upstream_status"
	case errors.Is(err, ErrTransport):
		return "transport"
	default:
		return "unknown"
	}
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
