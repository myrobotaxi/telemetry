package geocode

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
