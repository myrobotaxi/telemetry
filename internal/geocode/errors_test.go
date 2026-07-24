package geocode

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// MYR-254 regression tests: geocode.ReverseGeocode used to build error
// strings that embedded the full-precision queried coordinates, the full
// request URL (including the access_token query param), and up to 256B of
// the raw Mapbox response body. Those errors were logged verbatim at five
// call sites in internal/store (see writer_drives.go, etc.), leaking a P1
// secret and P1 GPS data into production logs. These tests pin that every
// error ReverseGeocode returns is free of all three, across every failure
// mode the method can hit.

// secretToken is shaped like a real Mapbox public token so a regression
// that leaks it into an error string is impossible to miss in a failing
// test's diff.
const secretToken = "pk.super-secret-mapbox-token-do-not-leak" //nolint:gosec // test fixture, not a real credential

// coordinatePairPattern matches a "lat,lng"-shaped digit run with at
// least three decimal places on each side — the shape the pre-MYR-254
// code interpolated into error strings via fmt's %.4f/%f verbs (e.g.
// "30.2672,-97.7431" or "33.086000,-96.852200"). A sanitized error must
// never contain this pattern.
var coordinatePairPattern = regexp.MustCompile(`-?\d{1,3}\.\d{3,}\s*,\s*-?\d{1,3}\.\d{3,}`)

// assertErrorSanitized fails the test if err's message contains anything
// MYR-254 identified as a leak: a coordinate-pair-shaped digit run, the
// "access_token" query key (or the raw secret value), or an http(s) URL
// scheme (i.e. the request URL itself).
func assertErrorSanitized(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("assertErrorSanitized: err is nil")
	}
	msg := err.Error()

	if coordinatePairPattern.MatchString(msg) {
		t.Errorf("error string contains a coordinate-pair-shaped digit run: %q", msg)
	}
	if strings.Contains(strings.ToLower(msg), "access_token") {
		t.Errorf("error string contains %q: %q", "access_token", msg)
	}
	if strings.Contains(msg, secretToken) {
		t.Errorf("error string contains the raw secret token: %q", msg)
	}
	if strings.Contains(msg, "http://") || strings.Contains(msg, "https://") {
		t.Errorf("error string contains a URL scheme: %q", msg)
	}
}

func TestReverseGeocode_InvalidCoordinateSanitized(t *testing.T) {
	g := &MapboxGeocoder{token: secretToken, client: http.DefaultClient}
	_, err := g.ReverseGeocode(context.Background(), 999.0, 999.0)
	if !errors.Is(err, ErrInvalidCoordinate) {
		t.Fatalf("expected ErrInvalidCoordinate, got: %v", err)
	}
	assertErrorSanitized(t, err)
}

// TestReverseGeocode_TransportErrorSanitized simulates the exact failure
// mode named in MYR-254: (*http.Client).Do returning a *url.Error, whose
// Error() embeds the full request URL. The handler hijacks and closes the
// connection without writing a response, forcing a genuine transport
// error rather than a clean non-200 status.
func TestReverseGeocode_TransportErrorSanitized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	g := &MapboxGeocoder{token: secretToken, client: srv.Client()}
	g.client.Transport = &rewriteTransport{base: srv.Client().Transport, baseURL: srv.URL}

	_, err := g.ReverseGeocode(context.Background(), queryLat, queryLng)
	if err == nil {
		t.Fatal("expected a transport error, got nil")
	}
	if !errors.Is(err, ErrTransport) {
		t.Errorf("expected ErrTransport, got: %v", err)
	}
	assertErrorSanitized(t, err)
}

// TestReverseGeocode_ConnectionRefusedSanitized exercises a different
// *url.Error shape (a dial failure against a closed listener, rather than
// a hijacked-and-dropped connection) through the same sanitization path.
func TestReverseGeocode_ConnectionRefusedSanitized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close()

	g := &MapboxGeocoder{token: secretToken, client: http.DefaultClient}
	g.client.Transport = &rewriteTransport{base: http.DefaultTransport, baseURL: addr}

	_, err := g.ReverseGeocode(context.Background(), queryLat, queryLng)
	if err == nil {
		t.Fatal("expected a transport error, got nil")
	}
	if !errors.Is(err, ErrTransport) {
		t.Errorf("expected ErrTransport, got: %v", err)
	}
	assertErrorSanitized(t, err)
}

// TestReverseGeocode_UpstreamStatusSanitized covers a non-200 response
// whose body deliberately echoes back a coordinate pair and the
// access_token — mirroring how a real upstream error page can reflect
// query parameters — to confirm the sanitization holds even when the
// upstream itself hands back sensitive data.
func TestReverseGeocode_UpstreamStatusSanitized(t *testing.T) {
	leakyBody := `{"message":"invalid request","query":"33.086000,-96.852200","access_token":"` + secretToken + `"}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(leakyBody))
	}))
	defer srv.Close()

	g := &MapboxGeocoder{token: secretToken, client: srv.Client()}
	g.client.Transport = &rewriteTransport{base: srv.Client().Transport, baseURL: srv.URL}

	_, err := g.ReverseGeocode(context.Background(), queryLat, queryLng)
	if !errors.Is(err, ErrUpstreamStatus) {
		t.Fatalf("expected ErrUpstreamStatus, got: %v", err)
	}
	assertErrorSanitized(t, err)
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("expected the numeric status code in the error, got: %v", err)
	}
}

// TestReverseGeocode_LargeBodySanitized uses a body well over the 256B
// the old code truncated to (and still stuffed with a coordinate pair and
// the token) to confirm no prefix of the body leaks into the error
// either.
func TestReverseGeocode_LargeBodySanitized(t *testing.T) {
	big := strings.Repeat("x", 400)
	leakyBody := `{"message":"` + big + `","query":"33.086000,-96.852200","access_token":"` + secretToken + `"}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(leakyBody))
	}))
	defer srv.Close()

	g := &MapboxGeocoder{token: secretToken, client: srv.Client()}
	g.client.Transport = &rewriteTransport{base: srv.Client().Transport, baseURL: srv.URL}

	_, err := g.ReverseGeocode(context.Background(), queryLat, queryLng)
	if !errors.Is(err, ErrUpstreamStatus) {
		t.Fatalf("expected ErrUpstreamStatus, got: %v", err)
	}
	assertErrorSanitized(t, err)
	if strings.Contains(err.Error(), big) {
		t.Error("error string contains (a prefix of) the raw response body")
	}
}

func TestReverseGeocode_RateLimitedSanitized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"message":"rate limited","access_token":"` + secretToken + `"}`))
	}))
	defer srv.Close()

	g := &MapboxGeocoder{token: secretToken, client: srv.Client()}
	g.client.Transport = &rewriteTransport{base: srv.Client().Transport, baseURL: srv.URL}

	_, err := g.ReverseGeocode(context.Background(), queryLat, queryLng)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited, got: %v", err)
	}
	assertErrorSanitized(t, err)
}

func TestClassifyError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", err: nil, want: ""},
		{name: "invalid coordinate", err: fmt.Errorf("geocode.ReverseGeocode: %w", ErrInvalidCoordinate), want: "invalid_coordinate"},
		{name: "no result", err: ErrNoResult, want: "no_result"},
		{name: "rate limited", err: fmt.Errorf("geocode.ReverseGeocode: %w", ErrRateLimited), want: "rate_limited"},
		{name: "upstream status", err: fmt.Errorf("geocode.ReverseGeocode: status 500: %w", ErrUpstreamStatus), want: "upstream_status"},
		{name: "transport", err: fmt.Errorf("geocode.ReverseGeocode: %w", ErrTransport), want: "transport"},
		{name: "unrecognized error", err: errors.New("some other failure"), want: "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyError(tt.err); got != tt.want {
				t.Errorf("ClassifyError(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}
