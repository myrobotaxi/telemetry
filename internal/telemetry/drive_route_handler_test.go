package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// --- Test double for DriveRouteFetcher ---

type stubDriveRouteFetcher struct {
	data DriveRouteData
	err  error
}

func (s *stubDriveRouteFetcher) GetDriveRoute(_ context.Context, _ string) (DriveRouteData, error) {
	if s.err != nil {
		return DriveRouteData{}, s.err
	}
	return s.data, nil
}

func TestDriveRouteHandler_ServeHTTP(t *testing.T) {
	const (
		driveID = "clmno9876543210zyxw0001"
		userID  = "user-123"
		authOK  = "Bearer valid-token"
	)
	routeJSON := json.RawMessage(
		`[{"lat":32.98246,"lng":-96.90867,"speed":0,"heading":357,"timestamp":"2026-06-06T18:22:00Z"}]`,
	)

	tests := []struct {
		name            string
		authHeader      string
		tokenValidator  *stubTokenValidator
		reader          *stubVehicleSnapshotReader
		fetcher         *stubDriveRouteFetcher
		wantStatus      int
		wantErrCode     wserrors.ErrorCode
		wantRoutePoints string // happy-path: expected routePoints JSON
	}{
		{
			name:           "missing Authorization header",
			authHeader:     "",
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			fetcher:        &stubDriveRouteFetcher{},
			wantStatus:     http.StatusUnauthorized,
			wantErrCode:    wserrors.ErrCodeAuthFailed,
		},
		{
			name:           "invalid token",
			authHeader:     authOK,
			tokenValidator: &stubTokenValidator{err: errors.New("expired")},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			fetcher:        &stubDriveRouteFetcher{},
			wantStatus:     http.StatusUnauthorized,
			wantErrCode:    wserrors.ErrCodeAuthFailed,
		},
		{
			name:           "drive not found",
			authHeader:     authOK,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			fetcher:        &stubDriveRouteFetcher{err: fmt.Errorf("get drive by id: %w", sdk.ErrNotFound)},
			wantStatus:     http.StatusNotFound,
			wantErrCode:    wserrors.ErrCodeNotFound,
		},
		{
			name:           "drive fetch internal error",
			authHeader:     authOK,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			fetcher:        &stubDriveRouteFetcher{err: errors.New("db boom")},
			wantStatus:     http.StatusInternalServerError,
			wantErrCode:    wserrors.ErrCodeInternalError,
		},
		{
			name:           "ownership mismatch",
			authHeader:     authOK,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow("other-user")},
			fetcher: &stubDriveRouteFetcher{data: DriveRouteData{
				DriveID: driveID, VehicleID: fixtureSnapshotRowID, RoutePoints: routeJSON,
			}},
			wantStatus:  http.StatusForbidden,
			wantErrCode: wserrors.ErrCodeVehicleNotOwned,
		},
		{
			name:           "drive's vehicle missing is an internal fault",
			authHeader:     authOK,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{err: fmt.Errorf("get vehicle by id: %w", sdk.ErrNotFound)},
			fetcher: &stubDriveRouteFetcher{data: DriveRouteData{
				DriveID: driveID, VehicleID: "missing", RoutePoints: routeJSON,
			}},
			wantStatus:  http.StatusInternalServerError,
			wantErrCode: wserrors.ErrCodeInternalError,
		},
		{
			name:           "happy path returns the polyline",
			authHeader:     authOK,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			fetcher: &stubDriveRouteFetcher{data: DriveRouteData{
				DriveID: driveID, VehicleID: fixtureSnapshotRowID, RoutePoints: routeJSON,
			}},
			wantStatus:      http.StatusOK,
			wantRoutePoints: string(routeJSON),
		},
		{
			name:           "empty route serializes as []",
			authHeader:     authOK,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			fetcher: &stubDriveRouteFetcher{data: DriveRouteData{
				DriveID: driveID, VehicleID: fixtureSnapshotRowID, RoutePoints: nil,
			}},
			wantStatus:      http.StatusOK,
			wantRoutePoints: "[]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewDriveRouteHandler(
				tt.tokenValidator,
				tt.reader,
				tt.fetcher,
				discardLogger(),
			)

			mux := http.NewServeMux()
			mux.Handle("GET /api/drives/{driveId}/route", h)

			req := httptest.NewRequestWithContext(
				context.Background(),
				http.MethodGet,
				"/api/drives/"+driveID+"/route",
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
				return
			}

			var resp struct {
				DriveID     string          `json:"driveId"`
				RoutePoints json.RawMessage `json:"routePoints"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if resp.DriveID != driveID {
				t.Errorf("driveId: got %q, want %q", resp.DriveID, driveID)
			}
			if string(resp.RoutePoints) != tt.wantRoutePoints {
				t.Errorf("routePoints: got %s, want %s", resp.RoutePoints, tt.wantRoutePoints)
			}
		})
	}
}
