package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// --- Test double for DriveDetailFetcher ---

type stubDriveDetailFetcher struct {
	data DriveDetailData
	err  error
}

func (s *stubDriveDetailFetcher) GetDriveDetail(_ context.Context, _ string) (DriveDetailData, error) {
	if s.err != nil {
		return DriveDetailData{}, s.err
	}
	return s.data, nil
}

// fixtureDriveDetail returns a fully-populated drive owned (via
// fixtureSnapshotRowID) by the caller. Values mirror the
// docs/contracts/fixtures/rest/drive_detail.json shape.
func fixtureDriveDetail(driveID string) DriveDetailData {
	return DriveDetailData{
		ID:               driveID,
		VehicleID:        fixtureSnapshotRowID,
		StartTime:        "2026-04-13T18:22:00Z",
		EndTime:          "2026-04-13T18:46:18Z",
		Date:             "2026-04-13",
		DistanceMiles:    12.4,
		DurationMinutes:  24,
		AvgSpeedMph:      30.5,
		MaxSpeedMph:      65.2,
		EnergyUsedKwh:    4.2,
		StartChargeLevel: 82,
		EndChargeLevel:   76,
		FsdMiles:         8.1,
		FsdPercentage:    65.3,
		Interventions:    1,
		StartLocation:    "Home",
		StartAddress:     "742 Evergreen Terrace, San Francisco, CA 94107",
		EndLocation:      "Whole Foods Market",
		EndAddress:       "399 4th Street, San Francisco, CA 94107",
		CreatedAt:        time.Date(2026, 4, 13, 18, 46, 19, 0, time.UTC),
	}
}

func TestDriveDetailHandler_ServeHTTP(t *testing.T) {
	const (
		driveID = "clmno9876543210zyxw0001"
		userID  = "user-123"
		authOK  = "Bearer valid-token"
	)

	tests := []struct {
		name           string
		authHeader     string
		tokenValidator *stubTokenValidator
		reader         *stubVehicleSnapshotReader
		fetcher        *stubDriveDetailFetcher
		wantStatus     int
		wantErrCode    wserrors.ErrorCode
	}{
		{
			name:           "missing Authorization header",
			authHeader:     "",
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			fetcher:        &stubDriveDetailFetcher{},
			wantStatus:     http.StatusUnauthorized,
			wantErrCode:    wserrors.ErrCodeAuthFailed,
		},
		{
			name:           "invalid token",
			authHeader:     authOK,
			tokenValidator: &stubTokenValidator{err: errors.New("expired")},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			fetcher:        &stubDriveDetailFetcher{},
			wantStatus:     http.StatusUnauthorized,
			wantErrCode:    wserrors.ErrCodeAuthFailed,
		},
		{
			name:           "drive not found",
			authHeader:     authOK,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			fetcher:        &stubDriveDetailFetcher{err: fmt.Errorf("get drive by id: %w", sdk.ErrNotFound)},
			wantStatus:     http.StatusNotFound,
			wantErrCode:    wserrors.ErrCodeNotFound,
		},
		{
			name:           "drive fetch internal error",
			authHeader:     authOK,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			fetcher:        &stubDriveDetailFetcher{err: errors.New("db boom")},
			wantStatus:     http.StatusInternalServerError,
			wantErrCode:    wserrors.ErrCodeInternalError,
		},
		{
			name:           "ownership mismatch",
			authHeader:     authOK,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow("other-user")},
			fetcher:        &stubDriveDetailFetcher{data: fixtureDriveDetail(driveID)},
			wantStatus:     http.StatusForbidden,
			wantErrCode:    wserrors.ErrCodeVehicleNotOwned,
		},
		{
			name:           "drive's vehicle missing is an internal fault",
			authHeader:     authOK,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{err: fmt.Errorf("get vehicle by id: %w", sdk.ErrNotFound)},
			fetcher:        &stubDriveDetailFetcher{data: fixtureDriveDetail(driveID)},
			wantStatus:     http.StatusInternalServerError,
			wantErrCode:    wserrors.ErrCodeInternalError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewDriveDetailHandler(
				tt.tokenValidator,
				tt.reader,
				tt.fetcher,
				discardLogger(),
			)

			mux := http.NewServeMux()
			mux.Handle("GET /api/drives/{driveId}", h)

			req := httptest.NewRequestWithContext(
				context.Background(),
				http.MethodGet,
				"/api/drives/"+driveID,
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

			var env wserrors.ErrorEnvelope
			if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
				t.Fatalf("decode error envelope: %v", err)
			}
			if env.Error.Code != tt.wantErrCode {
				t.Errorf("error.code: got %q, want %q", env.Error.Code, tt.wantErrCode)
			}
		})
	}
}

// TestDriveDetailHandler_HappyPath asserts the 200 body carries every
// DriveDetail wire field (fixture field set) with the expected values,
// including the durationSeconds conversion and RFC3339 createdAt.
func TestDriveDetailHandler_HappyPath(t *testing.T) {
	const (
		driveID = "clmno9876543210zyxw0001"
		userID  = "user-123"
	)

	h := NewDriveDetailHandler(
		&stubTokenValidator{userID: userID},
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
		&stubDriveDetailFetcher{data: fixtureDriveDetail(driveID)},
		discardLogger(),
	)

	mux := http.NewServeMux()
	mux.Handle("GET /api/drives/{driveId}", h)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/drives/"+driveID, nil)
	req.Header.Set("Authorization", "Bearer valid-token")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200. Body: %s", rec.Code, rec.Body.String())
	}

	var got map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	// Every field the fixture / OpenAPI DriveDetail declares must be
	// present — routePoints must NOT be (served by the route endpoint).
	wantKeys := []string{
		"id", "vehicleId", "startTime", "endTime", "date",
		"distanceMiles", "durationSeconds", "avgSpeedMph", "maxSpeedMph",
		"energyUsedKwh", "startChargeLevel", "endChargeLevel",
		"fsdMiles", "fsdPercentage", "interventions",
		"startLocation", "startAddress", "endLocation", "endAddress",
		"createdAt",
	}
	for _, k := range wantKeys {
		if _, ok := got[k]; !ok {
			t.Errorf("response missing key %q", k)
		}
	}
	if _, ok := got["routePoints"]; ok {
		t.Error("response must NOT include routePoints (served by /route)")
	}
	if len(got) != len(wantKeys) {
		t.Errorf("response key count: got %d, want %d (keys=%v)", len(got), len(wantKeys), sortedMapKeys(got))
	}

	// Spot-check load-bearing conversions and identity fields.
	if got["id"] != driveID {
		t.Errorf("id: got %v, want %q", got["id"], driveID)
	}
	if got["vehicleId"] != fixtureSnapshotRowID {
		t.Errorf("vehicleId: got %v, want %q", got["vehicleId"], fixtureSnapshotRowID)
	}
	if got["durationSeconds"] != float64(24*60) {
		t.Errorf("durationSeconds: got %v, want %d", got["durationSeconds"], 24*60)
	}
	if got["date"] != "2026-04-13" {
		t.Errorf("date: got %v, want 2026-04-13", got["date"])
	}
	if got["createdAt"] != "2026-04-13T18:46:19Z" {
		t.Errorf("createdAt: got %v, want 2026-04-13T18:46:19Z", got["createdAt"])
	}
}

// TestDriveDetailHandler_MissingDriveID asserts an empty driveId path
// value yields 400 invalid_request. The bare "/api/drives/" pattern is
// registered directly so the {driveId} wildcard resolves to "".
func TestDriveDetailHandler_MissingDriveID(t *testing.T) {
	h := NewDriveDetailHandler(
		&stubTokenValidator{userID: "user-123"},
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow("user-123")},
		&stubDriveDetailFetcher{},
		discardLogger(),
	)

	mux := http.NewServeMux()
	mux.Handle("GET /api/drives/{driveId}", h)
	// A trailing-slash request leaves {driveId} empty.
	mux.Handle("GET /api/drives/", h)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/drives/", nil)
	req.Header.Set("Authorization", "Bearer valid-token")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400. Body: %s", rec.Code, rec.Body.String())
	}
	var env wserrors.ErrorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if env.Error.Code != wserrors.ErrCodeInvalidRequest {
		t.Errorf("error.code: got %q, want %q", env.Error.Code, wserrors.ErrCodeInvalidRequest)
	}
}

// sortedMapKeys returns the keys of m for test diagnostics.
func sortedMapKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
