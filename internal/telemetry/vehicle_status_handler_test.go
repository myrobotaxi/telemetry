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

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/mask"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
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
				var errResp wserrors.ErrorEnvelope
				if err := json.NewDecoder(rec.Body).Decode(&errResp); err != nil {
					t.Fatalf("decode error response: %v", err)
				}
				if !strings.Contains(errResp.Error.Message, tt.wantError) {
					t.Errorf("error message: got %q, want substring %q", errResp.Error.Message, tt.wantError)
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

// TestVehicleStatusResponse_ToMaskMap_PreservesWireNames verifies the
// typed projection returns a map keyed by JSON wire names — the same
// keys the mask matrix uses. Replaces the structToMap round-trip test
// removed in MYR-58.
func TestVehicleStatusResponse_ToMaskMap_PreservesWireNames(t *testing.T) {
	lastMsg := "2026-04-30T12:00:00Z"
	connSince := "2026-04-30T11:00:00Z"
	in := vehicleStatusResponse{
		VIN:            "5YJ3E1EA1PF000001",
		Connected:      true,
		LastMessageAt:  &lastMsg,
		MessageCount:   42,
		ConnectedSince: &connSince,
	}
	got := in.ToMaskMap()

	wantKeys := []string{"vin", "connected", "last_message_at", "message_count", "connected_since"}
	for _, k := range wantKeys {
		if _, ok := got[k]; !ok {
			t.Errorf("missing key %q in %v", k, got)
		}
	}
	if len(got) != len(wantKeys) {
		t.Errorf("unexpected keys: got %v, want exactly %v", got, wantKeys)
	}

	// Pointer-typed fields flatten to their pointed-to value.
	if got["last_message_at"] != lastMsg {
		t.Errorf("last_message_at: got %v, want %q", got["last_message_at"], lastMsg)
	}
	if got["connected_since"] != connSince {
		t.Errorf("connected_since: got %v, want %q", got["connected_since"], connSince)
	}
}

// TestVehicleStatusResponse_ToMaskMap_NilPointersFlattenToNil verifies
// that nil pointer fields produce a `nil` map value (not the absence
// of the key) — this matches the post-Marshal/Unmarshal shape the old
// structToMap helper produced, so the projected JSON output stays
// byte-identical to the pre-MYR-58 implementation.
func TestVehicleStatusResponse_ToMaskMap_NilPointersFlattenToNil(t *testing.T) {
	in := vehicleStatusResponse{
		VIN:       "5YJ3E1EA1PF000001",
		Connected: false,
	}
	got := in.ToMaskMap()

	if v, ok := got["last_message_at"]; !ok || v != nil {
		t.Errorf("last_message_at: got (%v, ok=%v), want (nil, ok=true)", v, ok)
	}
	if v, ok := got["connected_since"]; !ok || v != nil {
		t.Errorf("connected_since: got (%v, ok=%v), want (nil, ok=true)", v, ok)
	}
	if got["message_count"] != int64(0) {
		t.Errorf("message_count zero: got %v (%T), want int64(0)", got["message_count"], got["message_count"])
	}
}

// BenchmarkVehicleStatusResponse_ToMaskMap_VsJSONRoundTrip is the
// MYR-58 baseline → post-fix comparison. The "JSONRoundTrip" sub-bench
// reproduces the pre-MYR-58 structToMap implementation inline so the
// regression target stays self-contained even after the helper is
// removed. The "ToMaskMap" sub-bench measures the typed alternative.
//
// Acceptance criterion: ToMaskMap allocs ≤ 70% of JSONRoundTrip allocs.
// Run with: go test ./internal/telemetry -run=^$ -bench=ToMaskMap_VsJSONRoundTrip -benchmem
func BenchmarkVehicleStatusResponse_ToMaskMap_VsJSONRoundTrip(b *testing.B) {
	lastMsg := "2026-04-30T12:00:00Z"
	connSince := "2026-04-30T11:00:00Z"
	in := vehicleStatusResponse{
		VIN:            "5YJ3E1EA1PF000001",
		Connected:      true,
		LastMessageAt:  &lastMsg,
		MessageCount:   42,
		ConnectedSince: &connSince,
	}

	b.Run("JSONRoundTrip", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			encoded, err := json.Marshal(in)
			if err != nil {
				b.Fatal(err)
			}
			out := make(map[string]any)
			if err := json.Unmarshal(encoded, &out); err != nil {
				b.Fatal(err)
			}
			_ = out
		}
	})

	b.Run("ToMaskMap", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			out := in.ToMaskMap()
			_ = out
		}
	})
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
