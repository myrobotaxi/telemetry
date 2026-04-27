package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tnando/my-robo-taxi-telemetry/internal/auth"
	"github.com/tnando/my-robo-taxi-telemetry/internal/mask"
	"github.com/tnando/my-robo-taxi-telemetry/pkg/sdk"
)

// --- Test doubles for VehiclePresence ---

type stubVehiclePresence struct {
	info      ConnInfo
	connected bool
}

func (s *stubVehiclePresence) ConnectionInfo(_ string) (ConnInfo, bool) {
	return s.info, s.connected
}

// --- Tests ---

func TestVehicleStatusHandler_ServeHTTP(t *testing.T) {
	const (
		validVIN  = "5YJ3E1EA1PF000001"
		userID    = "user-123"
		authToken = "valid-token"
	)

	connectedSince := time.Date(2026, 3, 24, 10, 0, 0, 0, time.UTC)
	lastMessage := time.Date(2026, 3, 24, 10, 5, 30, 0, time.UTC)

	tests := []struct {
		name           string
		vin            string
		authHeader     string
		tokenValidator *stubTokenValidator
		vehicleOwner   *stubVehicleOwner
		presence       *stubVehiclePresence
		wantStatus     int
		wantError      string
		wantConnected  *bool // nil = skip check (error cases)
	}{
		{
			name:           "missing auth token",
			vin:            validVIN,
			authHeader:     "",
			tokenValidator: &stubTokenValidator{userID: userID},
			vehicleOwner:   &stubVehicleOwner{ownerID: userID},
			presence:       &stubVehiclePresence{},
			wantStatus:     http.StatusUnauthorized,
			wantError:      "missing Authorization header",
		},
		{
			name:           "invalid auth token",
			vin:            validVIN,
			authHeader:     "Bearer bad-token",
			tokenValidator: &stubTokenValidator{err: errors.New("token expired")},
			vehicleOwner:   &stubVehicleOwner{ownerID: userID},
			presence:       &stubVehiclePresence{},
			wantStatus:     http.StatusUnauthorized,
			wantError:      "invalid or expired token",
		},
		{
			name:           "invalid VIN (too short)",
			vin:            "SHORT",
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			vehicleOwner:   &stubVehicleOwner{ownerID: userID},
			presence:       &stubVehiclePresence{},
			wantStatus:     http.StatusBadRequest,
			wantError:      "invalid VIN: must be 17 characters",
		},
		{
			name:           "vehicle not found in DB",
			vin:            validVIN,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			vehicleOwner:   &stubVehicleOwner{err: fmt.Errorf("VehicleRepo.GetByVIN: %w", sdk.ErrNotFound)},
			presence:       &stubVehiclePresence{},
			wantStatus:     http.StatusNotFound,
			wantError:      "vehicle not found",
		},
		{
			name:           "VIN owned by different user",
			vin:            validVIN,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			vehicleOwner:   &stubVehicleOwner{ownerID: "other-user"},
			presence:       &stubVehiclePresence{},
			wantStatus:     http.StatusForbidden,
			wantError:      "you do not own this vehicle",
		},
		{
			name:           "vehicle not connected",
			vin:            validVIN,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			vehicleOwner:   &stubVehicleOwner{ownerID: userID},
			presence:       &stubVehiclePresence{connected: false},
			wantStatus:     http.StatusOK,
			wantConnected:  boolPtr(false),
		},
		{
			name:           "vehicle connected and streaming",
			vin:            validVIN,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			vehicleOwner:   &stubVehicleOwner{ownerID: userID},
			presence: &stubVehiclePresence{
				connected: true,
				info: ConnInfo{
					ConnectedSince: connectedSince,
					LastMessageAt:  lastMessage,
					MessageCount:   42,
				},
			},
			wantStatus:    http.StatusOK,
			wantConnected: boolPtr(true),
		},
		{
			name:           "vehicle connected but no messages yet",
			vin:            validVIN,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			vehicleOwner:   &stubVehicleOwner{ownerID: userID},
			presence: &stubVehiclePresence{
				connected: true,
				info: ConnInfo{
					ConnectedSince: connectedSince,
					LastMessageAt:  time.Time{}, // zero value
					MessageCount:   0,
				},
			},
			wantStatus:    http.StatusOK,
			wantConnected: boolPtr(true),
		},
		{
			name:           "vehicle lookup internal error",
			vin:            validVIN,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			vehicleOwner:   &stubVehicleOwner{err: errors.New("connection refused")},
			presence:       &stubVehiclePresence{},
			wantStatus:     http.StatusInternalServerError,
			wantError:      "internal error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := NewVehicleStatusHandler(
				tt.tokenValidator,
				tt.vehicleOwner,
				tt.presence,
				discardLogger(),
			)

			mux := http.NewServeMux()
			mux.Handle("GET /api/vehicle-status/{vin}", handler)

			req := httptest.NewRequestWithContext(
				context.Background(),
				http.MethodGet,
				"/api/vehicle-status/"+tt.vin,
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
				var errResp vehicleStatusErrorResponse
				if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
					t.Fatalf("decode error response: %v", err)
				}
				if !strings.Contains(errResp.Error, tt.wantError) {
					t.Errorf("error message: got %q, want substring %q", errResp.Error, tt.wantError)
				}
				return
			}

			var resp vehicleStatusResponse
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode success response: %v", err)
			}

			// VIN should always be redacted.
			wantVIN := redactVIN(tt.vin)
			if resp.VIN != wantVIN {
				t.Errorf("vin: got %q, want %q (redacted)", resp.VIN, wantVIN)
			}

			if tt.wantConnected != nil && resp.Connected != *tt.wantConnected {
				t.Errorf("connected: got %v, want %v", resp.Connected, *tt.wantConnected)
			}

			// Validate response shape based on connection state.
			if tt.wantConnected != nil && !*tt.wantConnected {
				if resp.ConnectedSince != nil {
					t.Errorf("connected_since: want nil when not connected, got %q", *resp.ConnectedSince)
				}
				if resp.LastMessageAt != nil {
					t.Errorf("last_message_at: want nil when not connected, got %q", *resp.LastMessageAt)
				}
				if resp.MessageCount != 0 {
					t.Errorf("message_count: want 0 when not connected, got %d", resp.MessageCount)
				}
			}

			if tt.wantConnected != nil && *tt.wantConnected {
				if resp.ConnectedSince == nil {
					t.Error("connected_since: want non-nil when connected, got nil")
				}
				if tt.presence.info.MessageCount > 0 && resp.LastMessageAt == nil {
					t.Error("last_message_at: want non-nil when messages received, got nil")
				}
				if tt.presence.info.LastMessageAt.IsZero() && resp.LastMessageAt != nil {
					t.Errorf("last_message_at: want nil when no messages, got %q", *resp.LastMessageAt)
				}
				if resp.MessageCount != tt.presence.info.MessageCount {
					t.Errorf("message_count: got %d, want %d", resp.MessageCount, tt.presence.info.MessageCount)
				}
			}
		})
	}
}

func boolPtr(v bool) *bool { return &v }

// --- Tests for the role-based field-mask plumbing ---

// stubRoleResolver returns a fixed role unless err is set.
type stubRoleResolver struct {
	role auth.Role
	err  error
}

func (s *stubRoleResolver) ResolveRole(_ context.Context, _, _ string) (auth.Role, error) {
	return s.role, s.err
}

// stubVehicleIDLookup returns a fixed vehicleID unless err is set.
type stubVehicleIDLookup struct {
	id  string
	err error
}

func (s *stubVehicleIDLookup) GetVehicleIDByVIN(_ context.Context, _ string) (string, error) {
	if s.err != nil {
		return "", s.err
	}
	return s.id, nil
}

// maskedResponseWithSynthetic is a tiny response shape that mirrors
// vehicleStatusResponse but adds a forward-looking `licensePlate`
// field. The actual vehicleStatusResponse doesn't carry licensePlate
// in v1 — this struct exercises the mask plumbing using a synthetic
// payload, validating that ANY licensePlate-bearing response would be
// projected correctly.
type maskedResponseWithSynthetic struct {
	VIN          string `json:"vin"`
	Connected    bool   `json:"connected"`
	LicensePlate string `json:"licensePlate"`
	Speed        int    `json:"speed"`
}

// TestStructToMap_PreservesWireNames verifies the JSON round-trip used
// by writeMaskedResponse produces a map keyed by JSON wire names — the
// same keys the mask matrix uses.
func TestStructToMap_PreservesWireNames(t *testing.T) {
	in := maskedResponseWithSynthetic{
		VIN:          "ABC",
		Connected:    true,
		LicensePlate: "XYZ-789",
		Speed:        65,
	}
	got, err := structToMap(in)
	if err != nil {
		t.Fatalf("structToMap: %v", err)
	}
	wantKeys := []string{"vin", "connected", "licensePlate", "speed"}
	for _, k := range wantKeys {
		if _, ok := got[k]; !ok {
			t.Errorf("missing key %q in %v", k, got)
		}
	}
}

// TestVehicleStatusHandler_MaskedResponse_RoleResolverError verifies
// the handler returns 500 when role resolution fails — fail-closed
// surfacing rather than silently degrading to a deny-all body.
func TestVehicleStatusHandler_MaskedResponse_RoleResolverError(t *testing.T) {
	handler := NewVehicleStatusHandler(
		&stubTokenValidator{userID: "user-1"},
		&stubVehicleOwner{ownerID: "user-1"},
		&stubVehiclePresence{},
		discardLogger(),
		WithMask(
			mask.ResourceVehicleState,
			&stubRoleResolver{err: errors.New("DB down")},
			&stubVehicleIDLookup{id: "veh-1"},
		),
	)

	mux := http.NewServeMux()
	mux.Handle("GET /api/vehicle-status/{vin}", handler)

	req := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"/api/vehicle-status/5YJ3E1EA1PF000001",
		nil,
	)
	req.Header.Set("Authorization", "Bearer t")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500", rec.Code)
	}
}

// TestVehicleStatusHandler_NoMaskOption_RawResponse verifies the
// backward-compatible path: when WithMask is NOT supplied, the handler
// emits the unmasked response (the response shape is a connectivity
// probe, NOT a canonical VehicleState — see the comment on
// writeMaskedResponse for why mask plumbing is opt-in here).
func TestVehicleStatusHandler_NoMaskOption_RawResponse(t *testing.T) {
	handler := NewVehicleStatusHandler(
		&stubTokenValidator{userID: "user-1"},
		&stubVehicleOwner{ownerID: "user-1"},
		&stubVehiclePresence{connected: false},
		discardLogger(),
	)

	mux := http.NewServeMux()
	mux.Handle("GET /api/vehicle-status/{vin}", handler)

	req := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"/api/vehicle-status/5YJ3E1EA1PF000001",
		nil,
	)
	req.Header.Set("Authorization", "Bearer t")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	// Should decode into the typed response struct cleanly (raw shape).
	var resp vehicleStatusResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode raw response: %v", err)
	}
}
