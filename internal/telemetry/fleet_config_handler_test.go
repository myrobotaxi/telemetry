package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// --- Test doubles ---

type stubTokenValidator struct {
	userID string
	err    error
}

func (s *stubTokenValidator) ValidateToken(_ context.Context, _ string) (string, error) {
	return s.userID, s.err
}

type stubVehicleOwner struct {
	ownerID string
	err     error
}

func (s *stubVehicleOwner) GetVehicleOwner(_ context.Context, _ string) (string, error) {
	return s.ownerID, s.err
}

type stubTeslaTokenProvider struct {
	token TeslaToken
	err   error
}

func (s *stubTeslaTokenProvider) GetTeslaToken(_ context.Context, _ string) (TeslaToken, error) {
	return s.token, s.err
}

// validTeslaToken returns a stub provider with a non-expired token.
func validTeslaToken() *stubTeslaTokenProvider {
	return &stubTeslaTokenProvider{
		token: TeslaToken{ //nolint:gosec // test fixture, not a real credential
			AccessToken: "tesla-oauth-token-abc",
			ExpiresAt:   time.Now().Add(time.Hour),
		},
	}
}

// stubFleetServer starts an httptest.Server that returns predefined responses.
// This lets us test the handler end-to-end through the real FleetAPIClient.
// The server is closed on test cleanup, so callers must NOT Close it again
// (double Close panics) — including the inline `stubFleetServer(t, ...).URL`
// callers that would otherwise leak.
func stubFleetServer(t *testing.T, statusCode int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newTestFleetClient creates a FleetAPIClient with zero retries for fast tests.
func newTestFleetClient(baseURL string) *FleetAPIClient {
	c := NewFleetAPIClient(FleetAPIConfig{BaseURL: baseURL}, discardLogger())
	c.retry.MaxRetries = 0
	return c
}

// --- Tests ---

func TestFleetConfigHandler_ServeHTTP(t *testing.T) {
	const (
		validVIN  = "5YJ3E1EA1PF000001"
		userID    = "user-123"
		authToken = "valid-token"
	)

	successBody := `{"response":{"updated_vehicles":1,"skipped_vehicles":{}}}`
	skippedBody := fmt.Sprintf(
		`{"response":{"updated_vehicles":0,"skipped_vehicles":{%q:"not_paired"}}}`,
		validVIN,
	)

	tests := []struct {
		name           string
		vin            string
		authHeader     string
		tokenValidator *stubTokenValidator
		vehicleOwner   *stubVehicleOwner
		teslaTokens    *stubTeslaTokenProvider
		fleetStatus    int
		fleetBody      string
		wantStatus     int
		wantError      string
	}{
		{
			name:           "successful config push",
			vin:            validVIN,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			vehicleOwner:   &stubVehicleOwner{ownerID: userID},
			teslaTokens:    validTeslaToken(),
			fleetStatus:    http.StatusOK,
			fleetBody:      successBody,
			wantStatus:     http.StatusOK,
		},
		{
			name:           "invalid VIN length",
			vin:            "SHORT",
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			vehicleOwner:   &stubVehicleOwner{ownerID: userID},
			teslaTokens:    validTeslaToken(),
			fleetStatus:    http.StatusOK,
			fleetBody:      successBody,
			wantStatus:     http.StatusBadRequest,
			wantError:      "invalid VIN: must be 17 characters",
		},
		{
			name:           "missing auth header",
			vin:            validVIN,
			authHeader:     "",
			tokenValidator: &stubTokenValidator{userID: userID},
			vehicleOwner:   &stubVehicleOwner{ownerID: userID},
			teslaTokens:    validTeslaToken(),
			fleetStatus:    http.StatusOK,
			fleetBody:      successBody,
			wantStatus:     http.StatusUnauthorized,
			wantError:      "missing Authorization header",
		},
		{
			name:           "malformed auth header (no Bearer prefix)",
			vin:            validVIN,
			authHeader:     "Basic " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			vehicleOwner:   &stubVehicleOwner{ownerID: userID},
			teslaTokens:    validTeslaToken(),
			fleetStatus:    http.StatusOK,
			fleetBody:      successBody,
			wantStatus:     http.StatusUnauthorized,
			wantError:      "missing Authorization header",
		},
		{
			name:           "invalid token",
			vin:            validVIN,
			authHeader:     "Bearer bad-token",
			tokenValidator: &stubTokenValidator{err: errors.New("token expired")},
			vehicleOwner:   &stubVehicleOwner{ownerID: userID},
			teslaTokens:    validTeslaToken(),
			fleetStatus:    http.StatusOK,
			fleetBody:      successBody,
			wantStatus:     http.StatusUnauthorized,
			wantError:      "invalid or expired token",
		},
		{
			name:           "vehicle not found",
			vin:            validVIN,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			vehicleOwner:   &stubVehicleOwner{err: fmt.Errorf("VehicleRepo.GetByVIN: %w", sdk.ErrNotFound)},
			teslaTokens:    validTeslaToken(),
			fleetStatus:    http.StatusOK,
			fleetBody:      successBody,
			wantStatus:     http.StatusNotFound,
			wantError:      "vehicle not found",
		},
		{
			name:           "user does not own vehicle",
			vin:            validVIN,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			vehicleOwner:   &stubVehicleOwner{ownerID: "other-user"},
			teslaTokens:    validTeslaToken(),
			fleetStatus:    http.StatusOK,
			fleetBody:      successBody,
			wantStatus:     http.StatusForbidden,
			wantError:      "you do not own this vehicle",
		},
		{
			name:           "Tesla token not found",
			vin:            validVIN,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			vehicleOwner:   &stubVehicleOwner{ownerID: userID},
			teslaTokens:    &stubTeslaTokenProvider{err: fmt.Errorf("AccountRepo.GetTeslaToken: %w", sdk.ErrNotFound)},
			fleetStatus:    http.StatusOK,
			fleetBody:      successBody,
			wantStatus:     http.StatusUnauthorized,
			wantError:      "Tesla account not linked",
		},
		{
			name:           "Tesla token expired",
			vin:            validVIN,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			vehicleOwner:   &stubVehicleOwner{ownerID: userID},
			teslaTokens: &stubTeslaTokenProvider{
				token: TeslaToken{ //nolint:gosec // test fixture
					AccessToken: "expired-tesla-token",
					ExpiresAt:   time.Now().Add(-time.Hour),
				},
			},
			fleetStatus: http.StatusOK,
			fleetBody:   successBody,
			wantStatus:  http.StatusUnauthorized,
			wantError:   "Tesla token expired",
		},
		{
			name:           "Tesla token lookup internal error",
			vin:            validVIN,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			vehicleOwner:   &stubVehicleOwner{ownerID: userID},
			teslaTokens:    &stubTeslaTokenProvider{err: errors.New("connection refused")},
			fleetStatus:    http.StatusOK,
			fleetBody:      successBody,
			wantStatus:     http.StatusInternalServerError,
			wantError:      "internal error",
		},
		{
			name:           "fleet API server error",
			vin:            validVIN,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			vehicleOwner:   &stubVehicleOwner{ownerID: userID},
			teslaTokens:    validTeslaToken(),
			fleetStatus:    http.StatusInternalServerError,
			fleetBody:      `{"error":"internal server error"}`,
			wantStatus:     http.StatusBadGateway,
			wantError:      "fleet API error",
		},
		{
			name:           "fleet API client error (422)",
			vin:            validVIN,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			vehicleOwner:   &stubVehicleOwner{ownerID: userID},
			teslaTokens:    validTeslaToken(),
			fleetStatus:    http.StatusUnprocessableEntity,
			fleetBody:      `{"error":"invalid config"}`,
			wantStatus:     http.StatusBadGateway,
			wantError:      "fleet API rejected request",
		},
		{
			name:           "vehicle skipped by fleet API",
			vin:            validVIN,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			vehicleOwner:   &stubVehicleOwner{ownerID: userID},
			teslaTokens:    validTeslaToken(),
			fleetStatus:    http.StatusOK,
			fleetBody:      skippedBody,
			wantStatus:     http.StatusConflict,
			wantError:      "vehicle skipped: not_paired",
		},
		{
			name:           "vehicle lookup internal error",
			vin:            validVIN,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			vehicleOwner:   &stubVehicleOwner{err: errors.New("connection refused")},
			teslaTokens:    validTeslaToken(),
			fleetStatus:    http.StatusOK,
			fleetBody:      successBody,
			wantStatus:     http.StatusInternalServerError,
			wantError:      "internal error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Start a stub Fleet API server for each test case.
			fleetSrv := stubFleetServer(t, tt.fleetStatus, tt.fleetBody)

			fleetClient := newTestFleetClient(fleetSrv.URL)

			handler := NewFleetConfigHandler(
				tt.tokenValidator,
				tt.vehicleOwner,
				tt.teslaTokens,
				fleetClient,
				EndpointConfig{
					Hostname: "telemetry.example.com",
					Port:     443,
					CA:       "-----BEGIN CERTIFICATE-----\nTEST\n-----END CERTIFICATE-----",
				},
				discardLogger(),
			)

			// Build the request using a mux to populate PathValue.
			mux := http.NewServeMux()
			mux.Handle("POST /api/fleet-config/{vin}", handler)

			req := httptest.NewRequestWithContext(
				context.Background(),
				http.MethodPost,
				"/api/fleet-config/"+tt.vin,
				nil,
			)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status code: got %d, want %d", rec.Code, tt.wantStatus)
			}

			if tt.wantError != "" {
				var errResp wserrors.ErrorEnvelope
				if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
					t.Fatalf("decode error response: %v", err)
				}
				if errResp.Error.Code == "" {
					t.Error("expected error field in response, got empty")
				}
				if !strings.Contains(errResp.Error.Message, tt.wantError) {
					t.Errorf("error message: got %q, want substring %q", errResp.Error.Message, tt.wantError)
				}
			} else {
				var resp fleetConfigResponse
				if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
					t.Fatalf("decode success response: %v", err)
				}
				if resp.Status != "configured" {
					t.Errorf("status: got %q, want %q", resp.Status, "configured")
				}
				wantVIN := redactVIN(tt.vin)
				if resp.VIN != wantVIN {
					t.Errorf("vin: got %q, want %q (redacted)", resp.VIN, wantVIN)
				}
			}
		})
	}
}

func TestFleetConfigHandler_TeslaTokenPassedToFleetAPI(t *testing.T) {
	const (
		validVIN    = "5YJ3E1EA1PF000001"
		userID      = "user-123"
		teslaToken  = "tesla-oauth-real-token" //nolint:gosec // test fixture, not a real credential
		successBody = `{"response":{"updated_vehicles":1,"skipped_vehicles":{}}}`
	)

	// Capture the Authorization header sent to the Fleet API.
	var capturedAuth string
	fleetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, successBody)
	}))
	t.Cleanup(fleetSrv.Close)

	handler := NewFleetConfigHandler(
		&stubTokenValidator{userID: userID},
		&stubVehicleOwner{ownerID: userID},
		&stubTeslaTokenProvider{
			token: TeslaToken{
				AccessToken: teslaToken,
				ExpiresAt:   time.Now().Add(time.Hour),
			},
		},
		newTestFleetClient(fleetSrv.URL),
		EndpointConfig{
			Hostname: "telemetry.example.com",
			Port:     443,
			CA:       "ca-cert",
		},
		discardLogger(),
	)

	mux := http.NewServeMux()
	mux.Handle("POST /api/fleet-config/{vin}", handler)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/fleet-config/"+validVIN, nil)
	req.Header.Set("Authorization", "Bearer internal-jwt-token")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status code: got %d, want %d", rec.Code, http.StatusOK)
	}

	// Verify the Fleet API received the Tesla OAuth token, not the internal JWT.
	wantAuth := "Bearer " + teslaToken
	if capturedAuth != wantAuth {
		t.Errorf("Fleet API Authorization header: got %q, want %q", capturedAuth, wantAuth)
	}
}

func TestFleetConfigHandler_TeslaTokenNoExpiry(t *testing.T) {
	const (
		validVIN    = "5YJ3E1EA1PF000001"
		userID      = "user-123"
		successBody = `{"response":{"updated_vehicles":1,"skipped_vehicles":{}}}`
	)

	// A token with zero ExpiresAt (no expiry info) should be accepted.
	handler := NewFleetConfigHandler(
		&stubTokenValidator{userID: userID},
		&stubVehicleOwner{ownerID: userID},
		&stubTeslaTokenProvider{
			token: TeslaToken{ //nolint:gosec // test fixture
				AccessToken: "token-no-expiry",
			},
		},
		newTestFleetClient(stubFleetServer(t, http.StatusOK, successBody).URL),
		EndpointConfig{
			Hostname: "telemetry.example.com",
			Port:     443,
			CA:       "ca-cert",
		},
		discardLogger(),
	)

	mux := http.NewServeMux()
	mux.Handle("POST /api/fleet-config/{vin}", handler)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/fleet-config/"+validVIN, nil)
	req.Header.Set("Authorization", "Bearer valid-jwt")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status code: got %d, want %d (token with no expiry should succeed)", rec.Code, http.StatusOK)
	}
}

func TestFleetConfigHandler_Status(t *testing.T) {
	const (
		validVIN  = "5YJ3E1EA1PF000001"
		userID    = "user-123"
		authToken = "valid-token"
	)

	futureExp := time.Now().Add(300 * 24 * time.Hour).Unix()
	pastExp := time.Now().Add(-24 * time.Hour).Unix()

	syncedFuture := fmt.Sprintf(`{"response":{"synced":true,"config":{"hostname":"telemetry.example.com","port":443,"exp":%d}}}`, futureExp)
	syncedExpired := fmt.Sprintf(`{"response":{"synced":true,"config":{"hostname":"h","port":443,"exp":%d}}}`, pastExp)
	notSynced := `{"response":{"synced":false,"config":null}}`
	syncedNoExp := `{"response":{"synced":true,"config":{"hostname":"h","port":443}}}`

	tests := []struct {
		name          string
		vin           string
		authHeader    string
		fleetStatus   int
		fleetBody     string
		wantStatus    int
		wantError     string
		wantSynced    bool
		wantExpired   bool
		wantHasExpiry bool
	}{
		{
			name: "synced with future expiry", vin: validVIN, authHeader: "Bearer " + authToken,
			fleetStatus: http.StatusOK, fleetBody: syncedFuture,
			wantStatus: http.StatusOK, wantSynced: true, wantHasExpiry: true,
		},
		{
			name: "synced but expired config", vin: validVIN, authHeader: "Bearer " + authToken,
			fleetStatus: http.StatusOK, fleetBody: syncedExpired,
			wantStatus: http.StatusOK, wantSynced: true, wantExpired: true, wantHasExpiry: true,
		},
		{
			name: "not synced, no config", vin: validVIN, authHeader: "Bearer " + authToken,
			fleetStatus: http.StatusOK, fleetBody: notSynced,
			wantStatus: http.StatusOK, wantSynced: false, wantHasExpiry: false,
		},
		{
			name: "synced but Tesla omitted exp", vin: validVIN, authHeader: "Bearer " + authToken,
			fleetStatus: http.StatusOK, fleetBody: syncedNoExp,
			wantStatus: http.StatusOK, wantSynced: true, wantHasExpiry: false,
		},
		{
			name: "invalid VIN", vin: "SHORT", authHeader: "Bearer " + authToken,
			fleetStatus: http.StatusOK, fleetBody: notSynced,
			wantStatus: http.StatusBadRequest, wantError: "invalid VIN",
		},
		{
			name: "fleet API error", vin: validVIN, authHeader: "Bearer " + authToken,
			fleetStatus: http.StatusInternalServerError, fleetBody: `{"error":"boom"}`,
			wantStatus: http.StatusBadGateway, wantError: "fleet API error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fleetSrv := stubFleetServer(t, tt.fleetStatus, tt.fleetBody)

			handler := NewFleetConfigHandler(
				&stubTokenValidator{userID: userID},
				&stubVehicleOwner{ownerID: userID},
				validTeslaToken(),
				newTestFleetClient(fleetSrv.URL),
				EndpointConfig{Hostname: "telemetry.example.com", Port: 443},
				discardLogger(),
			)

			mux := http.NewServeMux()
			mux.Handle("GET /api/fleet-config/{vin}", handler)

			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/fleet-config/"+tt.vin, nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status: got %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantError != "" {
				var errResp wserrors.ErrorEnvelope
				if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
					t.Fatalf("decode error response: %v", err)
				}
				if !strings.Contains(errResp.Error.Message, tt.wantError) {
					t.Errorf("error message: got %q, want substring %q", errResp.Error.Message, tt.wantError)
				}
				return
			}

			var resp fleetConfigStatusResponse
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode status response: %v", err)
			}
			if resp.Synced != tt.wantSynced {
				t.Errorf("synced: got %v, want %v", resp.Synced, tt.wantSynced)
			}
			if resp.Expired != tt.wantExpired {
				t.Errorf("expired: got %v, want %v", resp.Expired, tt.wantExpired)
			}
			if hasExpiry := resp.ExpiresAt != ""; hasExpiry != tt.wantHasExpiry {
				t.Errorf("has expiry: got %v, want %v (expiresAt=%q)", hasExpiry, tt.wantHasExpiry, resp.ExpiresAt)
			}
			// daysRemaining is set only when expiry is known AND not expired
			// (nil once expired so the UI branches on `expired`).
			switch {
			case tt.wantHasExpiry && !tt.wantExpired && resp.DaysRemaining == nil:
				t.Error("expected daysRemaining to be set when expiry is present and not expired")
			case tt.wantExpired && resp.DaysRemaining != nil:
				t.Errorf("expected daysRemaining nil when expired, got %d", *resp.DaysRemaining)
			}
		})
	}
}

func TestFleetConfigHandler_MethodNotAllowed(t *testing.T) {
	handler := NewFleetConfigHandler(
		&stubTokenValidator{userID: "user-123"},
		&stubVehicleOwner{ownerID: "user-123"},
		validTeslaToken(),
		newTestFleetClient(stubFleetServer(t, http.StatusOK, `{"response":{"synced":true}}`).URL),
		EndpointConfig{Hostname: "telemetry.example.com", Port: 443},
		discardLogger(),
	)

	// Register without a method so ServeHTTP's own dispatch handles the verb.
	mux := http.NewServeMux()
	mux.Handle("/api/fleet-config/{vin}", handler)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPut, "/api/fleet-config/5YJ3E1EA1PF000001", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status: got %d, want %d", rec.Code, http.StatusMethodNotAllowed)
	}
}

func TestFleetConfigHandler_StatusEnforcesOwnership(t *testing.T) {
	// GET shares authorize() with POST; guard that a non-owner is rejected
	// on the GET path so a future refactor can't regress it.
	handler := NewFleetConfigHandler(
		&stubTokenValidator{userID: "user-123"},
		&stubVehicleOwner{ownerID: "someone-else"},
		validTeslaToken(),
		newTestFleetClient(stubFleetServer(t, http.StatusOK, `{"response":{"synced":true}}`).URL),
		EndpointConfig{Hostname: "telemetry.example.com", Port: 443},
		discardLogger(),
	)

	mux := http.NewServeMux()
	mux.Handle("GET /api/fleet-config/{vin}", handler)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/fleet-config/5YJ3E1EA1PF000001", nil)
	req.Header.Set("Authorization", "Bearer valid-token")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("GET status: got %d, want %d (ownership must be enforced)", rec.Code, http.StatusForbidden)
	}
}

func TestExtractBearerToken(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{name: "valid bearer", header: "Bearer abc123", want: "abc123"},
		{name: "empty header", header: "", want: ""},
		{name: "basic auth", header: "Basic abc123", want: ""},
		{name: "bearer only", header: "Bearer ", want: ""},
		{name: "lowercase bearer", header: "bearer abc123", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
			if tt.header != "" {
				req.Header.Set("Authorization", tt.header)
			}
			got := extractBearerToken(req)
			if got != tt.want {
				t.Errorf("extractBearerToken(%q) = %q, want %q", tt.header, got, tt.want)
			}
		})
	}
}

func TestFleetConfigHandler_FleetAPIUnreachable(t *testing.T) {
	// Fleet API client pointed at a non-existent server.
	fleetClient := newTestFleetClient("http://127.0.0.1:1")

	handler := NewFleetConfigHandler(
		&stubTokenValidator{userID: "user-123"},
		&stubVehicleOwner{ownerID: "user-123"},
		validTeslaToken(),
		fleetClient,
		EndpointConfig{
			Hostname: "telemetry.example.com",
			Port:     443,
			CA:       "ca-cert",
		},
		discardLogger(),
	)

	mux := http.NewServeMux()
	mux.Handle("POST /api/fleet-config/{vin}", handler)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/api/fleet-config/5YJ3E1EA1PF000001", nil)
	req.Header.Set("Authorization", "Bearer valid-token")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("status code: got %d, want %d", rec.Code, http.StatusBadGateway)
	}

	var errResp wserrors.ErrorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if errResp.Error.Code == "" {
		t.Error("expected error field in response")
	}
}
