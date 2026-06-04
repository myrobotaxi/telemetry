package telemetry

import (
	"context"
	"encoding/base64"
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

// --- Test doubles for DriveLister ---

type stubDriveLister struct {
	page    DriveListPage
	err     error
	lastCur DriveListCursor
	lastLim int
}

func (s *stubDriveLister) ListByVehicleID(_ context.Context, _ string, cursor DriveListCursor, limit int) (DriveListPage, error) {
	s.lastCur = cursor
	s.lastLim = limit
	return s.page, s.err
}

// fixtureDriveItems returns a deterministic seed of n drives.
func fixtureDriveItems(vehicleID string, n int) []DriveListItem {
	base := time.Date(2026, 4, 13, 18, 22, 0, 0, time.UTC)
	out := make([]DriveListItem, 0, n)
	for i := 0; i < n; i++ {
		startTime := base.Add(-time.Duration(i) * time.Hour)
		out = append(out, DriveListItem{
			ID:               fmt.Sprintf("clmno9876543210zyxw%04d", i+1),
			VehicleID:        vehicleID,
			Date:             startTime.Format("2006-01-02"),
			StartTime:        startTime.Format(time.RFC3339),
			EndTime:          startTime.Add(24 * time.Minute).Format(time.RFC3339),
			DistanceMiles:    12.4,
			DurationMinutes:  24,
			AvgSpeedMph:      30.5,
			MaxSpeedMph:      65.2,
			StartChargeLevel: 82,
			EndChargeLevel:   76,
			CreatedAt:        startTime.Add(25 * time.Minute),
		})
	}
	return out
}

// --- Tests ---

func TestVehicleDrivesHandler_ServeHTTP(t *testing.T) {
	const (
		vehicleID = "clxyz1234567890abcdef"
		userID    = "user-123"
		authToken = "valid-token"
	)

	items := fixtureDriveItems(vehicleID, 3)

	tests := []struct {
		name           string
		queryString    string
		authHeader     string
		tokenValidator *stubTokenValidator
		reader         *stubVehicleSnapshotReader
		drives         *stubDriveLister
		wantStatus     int
		wantErrCode    wserrors.ErrorCode
		wantErrSubstr  string
		wantItemsLen   int
		wantHasMore    bool
		wantNextNil    bool
	}{
		{
			name:           "missing Authorization header",
			authHeader:     "",
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			drives:         &stubDriveLister{},
			wantStatus:     http.StatusUnauthorized,
			wantErrCode:    wserrors.ErrCodeAuthFailed,
			wantErrSubstr:  "missing Authorization header",
		},
		{
			name:           "invalid token returns 401",
			authHeader:     "Bearer bad",
			tokenValidator: &stubTokenValidator{err: errors.New("expired")},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			drives:         &stubDriveLister{},
			wantStatus:     http.StatusUnauthorized,
			wantErrCode:    wserrors.ErrCodeAuthFailed,
			wantErrSubstr:  "invalid or expired token",
		},
		{
			name:           "unknown vehicle returns 404",
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{err: fmt.Errorf("not found: %w", sdk.ErrNotFound)},
			drives:         &stubDriveLister{},
			wantStatus:     http.StatusNotFound,
			wantErrCode:    wserrors.ErrCodeNotFound,
			wantErrSubstr:  "vehicle not found",
		},
		{
			name:           "non-owner returns 403",
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow("other-user")},
			drives:         &stubDriveLister{},
			wantStatus:     http.StatusForbidden,
			wantErrCode:    wserrors.ErrCodeVehicleNotOwned,
			wantErrSubstr:  "you do not own this vehicle",
		},
		{
			name:           "vehicle lookup internal error returns 500",
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{err: errors.New("db down")},
			drives:         &stubDriveLister{},
			wantStatus:     http.StatusInternalServerError,
			wantErrCode:    wserrors.ErrCodeInternalError,
			wantErrSubstr:  "internal error",
		},
		{
			name:           "drive lister internal error returns 500",
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			drives:         &stubDriveLister{err: errors.New("query failed")},
			wantStatus:     http.StatusInternalServerError,
			wantErrCode:    wserrors.ErrCodeInternalError,
			wantErrSubstr:  "internal error",
		},
		{
			name:           "limit out of range returns 400",
			queryString:    "?limit=999",
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			drives:         &stubDriveLister{},
			wantStatus:     http.StatusBadRequest,
			wantErrCode:    wserrors.ErrCodeInvalidRequest,
			wantErrSubstr:  "limit",
		},
		{
			name:           "limit zero returns 400",
			queryString:    "?limit=0",
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			drives:         &stubDriveLister{},
			wantStatus:     http.StatusBadRequest,
			wantErrCode:    wserrors.ErrCodeInvalidRequest,
			wantErrSubstr:  "limit",
		},
		{
			name:           "malformed cursor returns 400",
			queryString:    "?cursor=not-base64!!!",
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			drives:         &stubDriveLister{},
			wantStatus:     http.StatusBadRequest,
			wantErrCode:    wserrors.ErrCodeInvalidRequest,
			wantErrSubstr:  "cursor",
		},
		{
			name:           "happy path 200 with items and no more pages",
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			drives:         &stubDriveLister{page: DriveListPage{Items: items, HasMore: false}},
			wantStatus:     http.StatusOK,
			wantItemsLen:   3,
			wantHasMore:    false,
			wantNextNil:    true,
		},
		{
			name:           "happy path 200 with hasMore and nextCursor",
			authHeader:     "Bearer " + authToken,
			tokenValidator: &stubTokenValidator{userID: userID},
			reader:         &stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
			drives:         &stubDriveLister{page: DriveListPage{Items: items, HasMore: true}},
			wantStatus:     http.StatusOK,
			wantItemsLen:   3,
			wantHasMore:    true,
			wantNextNil:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewVehicleDrivesHandler(
				tt.tokenValidator,
				tt.reader,
				tt.drives,
				discardLogger(),
			)

			mux := http.NewServeMux()
			mux.Handle("GET /api/vehicles/{vehicleId}/drives", h)

			req := httptest.NewRequestWithContext(
				context.Background(),
				http.MethodGet,
				"/api/vehicles/"+vehicleID+"/drives"+tt.queryString,
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

			var body struct {
				Items      []map[string]any `json:"items"`
				NextCursor *string          `json:"nextCursor"`
				HasMore    bool             `json:"hasMore"`
			}
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if len(body.Items) != tt.wantItemsLen {
				t.Errorf("items len: got %d, want %d", len(body.Items), tt.wantItemsLen)
			}
			if body.HasMore != tt.wantHasMore {
				t.Errorf("hasMore: got %v, want %v", body.HasMore, tt.wantHasMore)
			}
			if tt.wantNextNil && body.NextCursor != nil {
				t.Errorf("nextCursor: got %q, want nil", *body.NextCursor)
			}
			if !tt.wantNextNil && body.NextCursor == nil {
				t.Error("nextCursor: got nil, want non-nil")
			}

			if tt.wantItemsLen > 0 {
				first := body.Items[0]
				if first["id"] != items[0].ID {
					t.Errorf("items[0].id: got %v, want %q", first["id"], items[0].ID)
				}
				if first["vehicleId"] != vehicleID {
					t.Errorf("items[0].vehicleId: got %v, want %q", first["vehicleId"], vehicleID)
				}
				// durationMinutes (24) converts to durationSeconds (1440).
				if first["durationSeconds"] != float64(1440) {
					t.Errorf("items[0].durationSeconds: got %v, want 1440", first["durationSeconds"])
				}
				// startAddress / endAddress / endLocation are deliberately
				// NOT in the DriveSummary allow-list — they belong to the
				// drive-detail endpoint per rest-api.md §5.2.2.
				for _, denied := range []string{"startAddress", "startLocation", "endAddress", "endLocation", "energyUsedKwh", "fsdMiles", "fsdPercentage", "interventions", "routePoints"} {
					if _, ok := first[denied]; ok {
						t.Errorf("items[0]: unexpected field %q leaked into drive summary", denied)
					}
				}
			}
		})
	}
}

// TestVehicleDrivesHandler_CursorRoundTrip drives a paginated scan
// across two pages and confirms the second call passes the decoded
// cursor back to the lister.
func TestVehicleDrivesHandler_CursorRoundTrip(t *testing.T) {
	const (
		vehicleID = "clxyz1234567890abcdef"
		userID    = "user-1"
	)

	items := fixtureDriveItems(vehicleID, 2)
	drives := &stubDriveLister{page: DriveListPage{Items: items, HasMore: true}}

	h := NewVehicleDrivesHandler(
		&stubTokenValidator{userID: userID},
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(userID)},
		drives,
		discardLogger(),
	)
	mux := http.NewServeMux()
	mux.Handle("GET /api/vehicles/{vehicleId}/drives", h)

	// Page 1.
	req1 := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/vehicles/"+vehicleID+"/drives", nil)
	req1.Header.Set("Authorization", "Bearer t")
	rec1 := httptest.NewRecorder()
	mux.ServeHTTP(rec1, req1)

	if rec1.Code != http.StatusOK {
		t.Fatalf("page 1: got %d, want 200. Body: %s", rec1.Code, rec1.Body.String())
	}

	var page1 struct {
		Items      []map[string]any `json:"items"`
		NextCursor *string          `json:"nextCursor"`
		HasMore    bool             `json:"hasMore"`
	}
	if err := json.NewDecoder(rec1.Body).Decode(&page1); err != nil {
		t.Fatalf("decode page 1: %v", err)
	}
	if page1.NextCursor == nil {
		t.Fatal("page 1: expected non-nil nextCursor when hasMore=true")
	}
	if !drives.lastCur.IsZero() {
		t.Errorf("page 1: expected zero cursor on first call, got %+v", drives.lastCur)
	}
	if drives.lastLim != driveListDefaultLimit {
		t.Errorf("page 1: limit = %d, want default %d", drives.lastLim, driveListDefaultLimit)
	}

	// Verify the cursor encodes the last item's (startTime, id).
	raw, err := base64.StdEncoding.DecodeString(*page1.NextCursor)
	if err != nil {
		t.Fatalf("decode cursor base64: %v", err)
	}
	var decoded DriveListCursor
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode cursor json: %v", err)
	}
	last := items[len(items)-1]
	if decoded.StartTime != last.StartTime || decoded.ID != last.ID {
		t.Errorf("cursor: got (%q, %q), want (%q, %q)", decoded.StartTime, decoded.ID, last.StartTime, last.ID)
	}

	// Page 2: pass the cursor back; assert the lister sees the same
	// (startTime, id) anchor.
	req2 := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/vehicles/"+vehicleID+"/drives?cursor="+*page1.NextCursor+"&limit=5", nil)
	req2.Header.Set("Authorization", "Bearer t")
	drives.page = DriveListPage{Items: nil, HasMore: false}
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)

	if rec2.Code != http.StatusOK {
		t.Fatalf("page 2: got %d, want 200. Body: %s", rec2.Code, rec2.Body.String())
	}
	if drives.lastCur.StartTime != last.StartTime || drives.lastCur.ID != last.ID {
		t.Errorf("page 2 cursor delivered to lister: got %+v, want (%q, %q)", drives.lastCur, last.StartTime, last.ID)
	}
	if drives.lastLim != 5 {
		t.Errorf("page 2 limit: got %d, want 5", drives.lastLim)
	}
}

// TestDriveCursorRoundTrip exercises the encode/decode pair directly.
func TestDriveCursorRoundTrip(t *testing.T) {
	in := DriveListCursor{StartTime: "2026-04-13T08:14:00Z", ID: "clmno9876543210zyxw0002"}
	encoded := encodeDriveCursor(in)
	got, err := decodeDriveCursor(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got != in {
		t.Errorf("round-trip: got %+v, want %+v", got, in)
	}
}

func TestDecodeDriveCursor_RejectsMalformed(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"empty payload after decode", base64.StdEncoding.EncodeToString([]byte(`{}`))},
		{"missing id", base64.StdEncoding.EncodeToString([]byte(`{"startTime":"2026-04-13T08:14:00Z"}`))},
		{"missing startTime", base64.StdEncoding.EncodeToString([]byte(`{"id":"abc"}`))},
		{"invalid base64", "%%not-base64%%"},
		{"valid base64 but not JSON", base64.StdEncoding.EncodeToString([]byte("garbage"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := decodeDriveCursor(tt.in); !errors.Is(err, errMalformedCursor) {
				t.Errorf("got err=%v, want errMalformedCursor", err)
			}
		})
	}
}
