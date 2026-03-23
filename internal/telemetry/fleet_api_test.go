package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fleetTestLogger returns a silent logger for fleet API tests.
func fleetTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestFleetAPIClient_PushTelemetryConfig_Success(t *testing.T) {
	t.Parallel()

	want := FleetConfigResponse{
		Response: FleetConfigResult{
			UpdatedVehicles: 1,
			SkippedVehicles: map[string]string{},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/1/vehicles/fleet_telemetry_config" {
			t.Errorf("path = %q, want /api/1/vehicles/fleet_telemetry_config", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("auth header = %q, want %q", got, "Bearer test-token")
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("content-type = %q, want application/json", ct)
		}

		// Verify the request body is valid JSON with expected fields.
		var req FleetConfigRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request body: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if len(req.VINs) != 1 || req.VINs[0] != testVIN {
			t.Errorf("VINs = %v, want [%s]", req.VINs, testVIN)
		}
		if req.Config.Hostname != "telemetry.myrobotaxi.app" {
			t.Errorf("hostname = %q, want telemetry.myrobotaxi.app", req.Config.Hostname)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(want) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)

	client := NewFleetAPIClient(FleetAPIConfig{
		BaseURL: srv.URL,
	}, fleetTestLogger())

	req := FleetConfigRequest{
		VINs: []string{testVIN},
		Config: FleetConfig{
			Hostname:   "telemetry.myrobotaxi.app",
			Port:       443,
			CA:         "-----BEGIN CERTIFICATE-----\ntest\n-----END CERTIFICATE-----",
			Fields:     DefaultFieldConfig(),
			AlertTypes: []string{"service"},
		},
	}

	resp, err := client.PushTelemetryConfig(context.Background(), "test-token", req)
	if err != nil {
		t.Fatalf("PushTelemetryConfig() error = %v", err)
	}
	if resp.Response.UpdatedVehicles != 1 {
		t.Errorf("updated = %d, want 1", resp.Response.UpdatedVehicles)
	}
}

func TestFleetAPIClient_PushTelemetryConfig_NoVINs(t *testing.T) {
	t.Parallel()

	client := NewFleetAPIClient(FleetAPIConfig{
		BaseURL: "http://unused",
	}, fleetTestLogger())

	_, err := client.PushTelemetryConfig(context.Background(), "token", FleetConfigRequest{})
	if err == nil {
		t.Fatal("expected error for empty VINs, got nil")
	}
}

func TestFleetAPIClient_PushTelemetryConfig_RateLimitRetry(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := attempts.Add(1)
		if n <= 2 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error": "rate limited"}`)) //nolint:errcheck // test server
			return
		}

		resp := FleetConfigResponse{
			Response: FleetConfigResult{
				UpdatedVehicles: 1,
				SkippedVehicles: map[string]string{},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)

	client := NewFleetAPIClient(FleetAPIConfig{
		BaseURL:    srv.URL,
		MaxRetries: 3,
	}, fleetTestLogger())

	req := FleetConfigRequest{
		VINs: []string{testVIN},
		Config: FleetConfig{
			Hostname: "telemetry.myrobotaxi.app",
			Port:     443,
			Fields:   DefaultFieldConfig(),
		},
	}

	resp, err := client.PushTelemetryConfig(context.Background(), "token", req)
	if err != nil {
		t.Fatalf("PushTelemetryConfig() error = %v", err)
	}
	if resp.Response.UpdatedVehicles != 1 {
		t.Errorf("updated = %d, want 1", resp.Response.UpdatedVehicles)
	}
	if got := int(attempts.Load()); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestFleetAPIClient_PushTelemetryConfig_MaxRetriesExhausted(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error": "rate limited"}`)) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)

	client := NewFleetAPIClient(FleetAPIConfig{
		BaseURL:    srv.URL,
		MaxRetries: 2,
	}, fleetTestLogger())

	req := FleetConfigRequest{
		VINs: []string{testVIN},
		Config: FleetConfig{
			Hostname: "telemetry.myrobotaxi.app",
			Port:     443,
			Fields:   DefaultFieldConfig(),
		},
	}

	_, err := client.PushTelemetryConfig(context.Background(), "token", req)
	if err == nil {
		t.Fatal("expected error after max retries, got nil")
	}

	var apiErr *FleetAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected FleetAPIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", apiErr.StatusCode, http.StatusTooManyRequests)
	}

	// Initial attempt + 2 retries = 3 total attempts.
	if got := int(attempts.Load()); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestFleetAPIClient_PushTelemetryConfig_ServerError(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": "internal server error"}`)) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)

	client := NewFleetAPIClient(FleetAPIConfig{
		BaseURL:    srv.URL,
		MaxRetries: 1,
	}, fleetTestLogger())

	req := FleetConfigRequest{
		VINs: []string{testVIN},
		Config: FleetConfig{
			Hostname: "telemetry.myrobotaxi.app",
			Port:     443,
			Fields:   DefaultFieldConfig(),
		},
	}

	_, err := client.PushTelemetryConfig(context.Background(), "token", req)
	if err == nil {
		t.Fatal("expected error for 500, got nil")
	}

	var apiErr *FleetAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected FleetAPIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", apiErr.StatusCode, http.StatusInternalServerError)
	}

	// Initial attempt + 1 retry = 2 total attempts.
	if got := int(attempts.Load()); got != 2 {
		t.Errorf("attempts = %d, want 2 (1 + 1 retry)", got)
	}
}

func TestFleetAPIClient_PushTelemetryConfig_ClientError(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error": "forbidden"}`)) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)

	client := NewFleetAPIClient(FleetAPIConfig{
		BaseURL:    srv.URL,
		MaxRetries: 3,
	}, fleetTestLogger())

	req := FleetConfigRequest{
		VINs: []string{testVIN},
		Config: FleetConfig{
			Hostname: "telemetry.myrobotaxi.app",
			Port:     443,
			Fields:   DefaultFieldConfig(),
		},
	}

	_, err := client.PushTelemetryConfig(context.Background(), "token", req)
	if err == nil {
		t.Fatal("expected error for 403, got nil")
	}

	// Client errors (4xx except 429) should NOT be retried.
	if got := int(attempts.Load()); got != 1 {
		t.Errorf("attempts = %d, want 1 (no retry for 403)", got)
	}
}

func TestFleetAPIClient_PushTelemetryConfig_ContextCancelled(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error": "rate limited"}`)) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)

	client := NewFleetAPIClient(FleetAPIConfig{
		BaseURL:    srv.URL,
		MaxRetries: 3,
	}, fleetTestLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	t.Cleanup(cancel)

	req := FleetConfigRequest{
		VINs: []string{testVIN},
		Config: FleetConfig{
			Hostname: "telemetry.myrobotaxi.app",
			Port:     443,
			Fields:   DefaultFieldConfig(),
		},
	}

	_, err := client.PushTelemetryConfig(ctx, "token", req)
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}

func TestFleetAPIClient_GetTelemetryErrors_Success(t *testing.T) {
	t.Parallel()

	now := time.Now().Truncate(time.Second)
	want := FleetErrorsResponse{
		Response: FleetErrorsList{
			Errors: []FleetError{
				{
					Name:      "certificate_error",
					Message:   "TLS handshake failed: certificate expired",
					CreatedAt: now,
				},
				{
					Name:      "connection_timeout",
					Message:   "vehicle failed to connect within 30s",
					CreatedAt: now.Add(-5 * time.Minute),
				},
			},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("auth header = %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(want) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)

	client := NewFleetAPIClient(FleetAPIConfig{
		BaseURL: srv.URL,
	}, fleetTestLogger())

	resp, err := client.GetTelemetryErrors(context.Background(), "test-token", testVIN)
	if err != nil {
		t.Fatalf("GetTelemetryErrors() error = %v", err)
	}
	if len(resp.Response.Errors) != 2 {
		t.Errorf("errors count = %d, want 2", len(resp.Response.Errors))
	}
	if resp.Response.Errors[0].Name != "certificate_error" {
		t.Errorf("error[0].Name = %q, want certificate_error", resp.Response.Errors[0].Name)
	}
}

func TestFleetAPIClient_GetTelemetryErrors_InvalidVIN(t *testing.T) {
	t.Parallel()

	client := NewFleetAPIClient(FleetAPIConfig{
		BaseURL: "http://unused",
	}, fleetTestLogger())

	tests := []struct {
		name string
		vin  string
	}{
		{"empty", ""},
		{"too short", "ABC123"},
		{"too long", "12345678901234567890"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := client.GetTelemetryErrors(context.Background(), "token", tt.vin)
			if err == nil {
				t.Fatal("expected error for invalid VIN, got nil")
			}
		})
	}
}

func TestFleetAPIClient_GetTelemetryErrors_NotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error": "vehicle not found"}`)) //nolint:errcheck // test server
	}))
	t.Cleanup(srv.Close)

	client := NewFleetAPIClient(FleetAPIConfig{
		BaseURL: srv.URL,
	}, fleetTestLogger())

	_, err := client.GetTelemetryErrors(context.Background(), "token", testVIN)
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}

	var apiErr *FleetAPIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected FleetAPIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want %d", apiErr.StatusCode, http.StatusNotFound)
	}
}

func TestFleetAPIClient_DefaultConfig(t *testing.T) {
	t.Parallel()

	client := NewFleetAPIClient(FleetAPIConfig{}, fleetTestLogger())

	if client.baseURL != defaultFleetAPIBaseURL {
		t.Errorf("baseURL = %q, want %q", client.baseURL, defaultFleetAPIBaseURL)
	}
	if client.httpClient.Timeout != defaultFleetAPITimeout {
		t.Errorf("timeout = %v, want %v", client.httpClient.Timeout, defaultFleetAPITimeout)
	}
	if client.retry.MaxRetries != defaultMaxRetries {
		t.Errorf("maxRetries = %d, want %d", client.retry.MaxRetries, defaultMaxRetries)
	}
}

func TestFleetAPIClient_CustomConfig(t *testing.T) {
	t.Parallel()

	client := NewFleetAPIClient(FleetAPIConfig{
		BaseURL:    "https://custom-fleet.example.com",
		Timeout:    10 * time.Second,
		MaxRetries: 5,
	}, fleetTestLogger())

	if client.baseURL != "https://custom-fleet.example.com" {
		t.Errorf("baseURL = %q, want custom URL", client.baseURL)
	}
	if client.httpClient.Timeout != 10*time.Second {
		t.Errorf("timeout = %v, want 10s", client.httpClient.Timeout)
	}
	if client.retry.MaxRetries != 5 {
		t.Errorf("maxRetries = %d, want 5", client.retry.MaxRetries)
	}
}

func TestDefaultFieldConfig(t *testing.T) {
	t.Parallel()

	fields := DefaultFieldConfig()

	// Spot-check key fields with specific intervals and deltas.
	spotChecks := []struct {
		field        string
		wantInterval int
		wantHasDelta bool
		wantDelta    float64
	}{
		{FleetFieldVehicleSpeed, 2, false, 0},
		{FleetFieldLocation, 2, true, 10},
		{FleetFieldGpsHeading, 5, false, 0},
		{FleetFieldGear, 1, false, 0},
		{FleetFieldDetailedChargeState, 30, false, 0},
		{FleetFieldOdometer, 60, false, 0},
		{FleetFieldDestinationName, 30, false, 0},
		{FleetFieldFSDMilesSinceReset, 60, false, 0},
	}

	for _, tt := range spotChecks {
		t.Run(tt.field, func(t *testing.T) {
			t.Parallel()
			fc, ok := fields[tt.field]
			if !ok {
				t.Fatalf("field %q not found in default config", tt.field)
			}
			if fc.IntervalSeconds != tt.wantInterval {
				t.Errorf("interval = %d, want %d", fc.IntervalSeconds, tt.wantInterval)
			}
			if tt.wantHasDelta {
				if fc.MinimumDelta == nil {
					t.Fatal("expected minimum_delta to be set")
				}
				if *fc.MinimumDelta != tt.wantDelta {
					t.Errorf("minimum_delta = %f, want %f", *fc.MinimumDelta, tt.wantDelta)
				}
			} else if fc.MinimumDelta != nil {
				t.Errorf("expected minimum_delta to be nil, got %f", *fc.MinimumDelta)
			}
		})
	}

	// Verify every field has a positive interval.
	for name, fc := range fields {
		if fc.IntervalSeconds <= 0 {
			t.Errorf("field %q has non-positive interval: %d", name, fc.IntervalSeconds)
		}
	}
}

func TestFleetAPIError_Error(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		err        FleetAPIError
		wantSubstr string
	}{
		{
			name:       "rate limited",
			err:        FleetAPIError{StatusCode: 429, Body: "too many requests"},
			wantSubstr: "429",
		},
		{
			name:       "server error",
			err:        FleetAPIError{StatusCode: 500, Body: "internal"},
			wantSubstr: "500",
		},
		{
			name:       "forbidden",
			err:        FleetAPIError{StatusCode: 403, Body: "forbidden"},
			wantSubstr: "403",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			msg := tt.err.Error()
			if msg == "" {
				t.Fatal("Error() returned empty string")
			}
		})
	}
}

func TestRetryPolicy_RetryAfterHeader(t *testing.T) {
	t.Parallel()

	resp := &http.Response{
		Header: http.Header{},
	}
	resp.Header.Set("Retry-After", "5")

	delay := retryDelay(resp, 0, defaultRetryPolicy())
	if delay != 5*time.Second {
		t.Errorf("delay = %v, want 5s (from Retry-After header)", delay)
	}
}

func TestRetryPolicy_ExponentialBackoff(t *testing.T) {
	t.Parallel()

	policy := defaultRetryPolicy()

	tests := []struct {
		attempt int
		wantMin time.Duration // base * 2^attempt * 0.75
		wantMax time.Duration // base * 2^attempt * 1.25
	}{
		{0, 750 * time.Millisecond, 1250 * time.Millisecond},
		{1, 1500 * time.Millisecond, 2500 * time.Millisecond},
		{2, 3 * time.Second, 5 * time.Second},
		{3, 6 * time.Second, 10 * time.Second},
		{4, 12 * time.Second, 20 * time.Second},
	}

	for _, tt := range tests {
		delay := retryDelay(nil, tt.attempt, policy)
		if delay < tt.wantMin || delay > tt.wantMax {
			t.Errorf("attempt %d: delay = %v, want [%v, %v]", tt.attempt, delay, tt.wantMin, tt.wantMax)
		}
	}
}

func TestRetryPolicy_MaxDelayCap(t *testing.T) {
	t.Parallel()

	policy := retryPolicy{
		BaseDelay: 1 * time.Second,
		MaxDelay:  10 * time.Second,
	}

	// Attempt 5 would be 32s without cap. With jitter, max is 10s * 1.25 = 12.5s.
	delay := retryDelay(nil, 5, policy)
	if delay < 7500*time.Millisecond || delay > 12500*time.Millisecond {
		t.Errorf("delay = %v, want [7.5s, 12.5s] (capped at 10s ± 25%% jitter)", delay)
	}
}

func TestIsRetryable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		code int
		want bool
	}{
		{200, false},
		{400, false},
		{401, false},
		{403, false},
		{404, false},
		{429, true},
		{500, true},
		{502, true},
		{503, true},
	}

	for _, tt := range tests {
		if got := isRetryable(tt.code); got != tt.want {
			t.Errorf("isRetryable(%d) = %v, want %v", tt.code, got, tt.want)
		}
	}
}
