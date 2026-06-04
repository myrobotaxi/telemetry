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

	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// --- Test doubles for VehicleSnapshotReader ---

type stubVehicleSnapshotReader struct {
	row VehicleSnapshotRow
	err error
}

func (s *stubVehicleSnapshotReader) GetByID(_ context.Context, _ string) (VehicleSnapshotRow, error) {
	if s.err != nil {
		return VehicleSnapshotRow{}, s.err
	}
	return s.row, nil
}

// fixtureSnapshotRowID is the canonical vehicleId used by every
// snapshot-handler test fixture. Pinning it here keeps the unparam
// linter from flagging fixtureSnapshotRow's vehicleID param as
// constant — every caller passes the same value, by design.
const fixtureSnapshotRowID = "clxyz1234567890abcdef"

// fixtureSnapshotRow returns a populated snapshot row owned by
// ownerID. The vehicleId is pinned to fixtureSnapshotRowID. The
// values mirror the rest-api.md §7.1 example body where they overlap
// with the schema.
func fixtureSnapshotRow(ownerID string) VehicleSnapshotRow {
	gear := "P"
	chargeState := "Disconnected"
	return VehicleSnapshotRow{
		ID:                 fixtureSnapshotRowID,
		UserID:             ownerID,
		VIN:                "5YJ3E1EA1PF000001",
		Name:               "Stumpy",
		Model:              "Model 3",
		Year:               2024,
		Color:              "Midnight Silver Metallic",
		Status:             "parked",
		ChargeLevel:        78,
		EstimatedRange:     245,
		ChargeState:        &chargeState,
		Speed:              0,
		GearPosition:       &gear,
		Heading:            180,
		Latitude:           10.0,
		Longitude:          20.0,
		LocationName:       "Home",
		LocationAddress:    "123 Market St, San Francisco, CA",
		InteriorTemp:       68,
		ExteriorTemp:       55,
		OdometerMiles:      12458,
		FsdMilesSinceReset: 412.7,
		LastUpdated:        time.Date(2026, 5, 31, 18, 22, 1, 0, time.UTC),
	}
}

// --- Tests ---

func TestVehicleSnapshotHandler_ServeHTTP(t *testing.T) {
	const (
		vehicleID = "clxyz1234567890abcdef"
		userID    = "user-123"
		authToken = "valid-token"
	)

	tests := []struct {
		name           string
		vehicleIDPath  string
		authHeader     string
		tokenValidator *stubTokenValidator
		reader         *stubVehicleSnapshotReader
		wantStatus     int
		wantErrCode    wserrors.ErrorCode
		wantErrSubstr  string
	}{
		{
			name:           "missing Authorization header",
			vehicleIDPath:  vehicleID,
			authHeader:     "",
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			wantStatus:     http.StatusUnauthorized,
			wantErrCode:    wserrors.ErrCodeAuthFailed,
			wantErrSubstr:  "missing Authorization header",
		},
		{
			name:           "invalid token returns 401",
			vehicleIDPath:  vehicleID,
			authHeader:     "Bearer bad",
			tokenValidator: &stubTokenValidator{err: errors.New("token expired")},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			wantStatus:     http.StatusUnauthorized,
			wantErrCode:    wserrors.ErrCodeAuthFailed,
			wantErrSubstr:  "invalid or expired token",
		},
		{
			name:           "unknown vehicleId returns 404",
			vehicleIDPath:  vehicleID,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{err: fmt.Errorf("VehicleRepo.GetByID: %w", sdk.ErrNotFound)},
			wantStatus:     http.StatusNotFound,
			wantErrCode:    wserrors.ErrCodeNotFound,
			wantErrSubstr:  "vehicle not found",
		},
		{
			name:           "non-owner returns 403",
			vehicleIDPath:  vehicleID,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow("other-user")},
			wantStatus:     http.StatusForbidden,
			wantErrCode:    wserrors.ErrCodeVehicleNotOwned,
			wantErrSubstr:  "you do not own this vehicle",
		},
		{
			name:           "store internal error returns 500",
			vehicleIDPath:  vehicleID,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{err: errors.New("db down")},
			wantStatus:     http.StatusInternalServerError,
			wantErrCode:    wserrors.ErrCodeInternalError,
			wantErrSubstr:  "internal error",
		},
		{
			name:           "owner happy path returns 200",
			vehicleIDPath:  vehicleID,
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			wantStatus:     http.StatusOK,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewVehicleSnapshotHandler(tt.tokenValidator, tt.reader, discardLogger())

			mux := http.NewServeMux()
			mux.Handle("GET /api/vehicles/{vehicleId}/snapshot", h)

			req := httptest.NewRequestWithContext(
				context.Background(),
				http.MethodGet,
				"/api/vehicles/"+tt.vehicleIDPath+"/snapshot",
				nil,
			)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status: got %d, want %d. Body: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}

			if tt.wantErrCode != "" {
				var env wserrors.ErrorEnvelope
				if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
					t.Fatalf("decode error envelope: %v", err)
				}
				if env.Error.Code != tt.wantErrCode {
					t.Errorf("error.code: got %q, want %q", env.Error.Code, tt.wantErrCode)
				}
				if tt.wantErrSubstr != "" && !strings.Contains(env.Error.Message, tt.wantErrSubstr) {
					t.Errorf("error.message: got %q, want substring %q", env.Error.Message, tt.wantErrSubstr)
				}
				return
			}

			var body map[string]any
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if body["vehicleId"] != vehicleID {
				t.Errorf("vehicleId: got %v, want %q", body["vehicleId"], vehicleID)
			}
			if body["name"] != "Stumpy" {
				t.Errorf("name: got %v, want Stumpy", body["name"])
			}
			if body["model"] != "Model 3" {
				t.Errorf("model: got %v, want Model 3", body["model"])
			}
			if body["chargeLevel"] != float64(78) {
				t.Errorf("chargeLevel: got %v, want 78", body["chargeLevel"])
			}
			if body["lastUpdated"] != "2026-05-31T18:22:01Z" {
				t.Errorf("lastUpdated: got %v, want 2026-05-31T18:22:01Z", body["lastUpdated"])
			}
		})
	}
}

// TestVehicleSnapshotHandler_OwnerMaskIsIdentity confirms that with
// the role plumbing wired, an owner caller still sees every
// VehicleState wire field the mask allow-list permits. Mirrors the
// vehicles list handler's owner-projection test.
func TestVehicleSnapshotHandler_OwnerMaskIsIdentity(t *testing.T) {
	const (
		vehicleID = "clxyz1234567890abcdef"
		userID    = "user-1"
	)

	h := NewVehicleSnapshotHandler(
		&stubTokenValidator{userID: userID},
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
		discardLogger(),
		WithSnapshotRoleResolver(&stubRoleResolver{role: "owner"}),
	)

	mux := http.NewServeMux()
	mux.Handle("GET /api/vehicles/{vehicleId}/snapshot", h)

	req := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"/api/vehicles/"+vehicleID+"/snapshot",
		nil,
	)
	req.Header.Set("Authorization", "Bearer t")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Every required (non-null) snapshot field from rest-api.md §7.1
	// must be present after owner projection.
	requiredKeys := []string{
		"vehicleId", "name", "model", "year", "color", "status",
		"chargeLevel", "estimatedRange", "speed", "heading",
		"latitude", "longitude", "locationName", "locationAddress",
		"interiorTemp", "exteriorTemp", "odometerMiles",
		"fsdMilesSinceReset", "lastUpdated",
	}
	for _, k := range requiredKeys {
		if _, ok := body[k]; !ok {
			t.Errorf("owner projection: missing required field %q", k)
		}
	}
}

// TestVehicleSnapshotHandler_NoLatLngInLogs is a smoke check that an
// owner request body still encodes the lat/lng for the wire (so the
// SDK can render the snapshot) — paired with the contract-guard
// expectation that we never log P1 values. We do not log lat/lng in
// any handler path; this test pins the wire shape, not the log path.
func TestVehicleSnapshotHandler_LatLngOnWire(t *testing.T) {
	const (
		vehicleID = "clxyz1234567890abcdef"
		userID    = "user-1"
	)

	h := NewVehicleSnapshotHandler(
		&stubTokenValidator{userID: userID},
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
		discardLogger(),
	)

	mux := http.NewServeMux()
	mux.Handle("GET /api/vehicles/{vehicleId}/snapshot", h)

	req := httptest.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		"/api/vehicles/"+vehicleID+"/snapshot",
		nil,
	)
	req.Header.Set("Authorization", "Bearer t")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["latitude"] != 10.0 {
		t.Errorf("latitude: got %v, want 10.0", body["latitude"])
	}
	if body["longitude"] != 20.0 {
		t.Errorf("longitude: got %v, want 20.0", body["longitude"])
	}
}
