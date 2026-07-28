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

const (
	swUserID   = "usr_sw_owner"
	swOtherUsr = "usr_sw_stranger"
	swAuthOK   = "Bearer good-token"
)

// swNow pins "now" so the future-instant validation is deterministic.
var swNow = time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

// fakeOwnerServiceWindowWriter records the owner write, keeping nil (a clear)
// distinguishable from "never called".
type fakeOwnerServiceWindowWriter struct {
	calls  int
	lastAt *time.Time
	err    error
}

func (f *fakeOwnerServiceWindowWriter) SetServiceExpectedEndAt(_ context.Context, _ string, at *time.Time) error {
	f.calls++
	f.lastAt = at
	return f.err
}

func newServiceWindowHandler(
	reader *stubVehicleSnapshotReader,
	writer *fakeOwnerServiceWindowWriter,
	userID string,
) *VehicleServiceWindowHandler {
	return NewVehicleServiceWindowHandler(
		&stubTokenValidator{userID: userID},
		reader,
		writer,
		discardLogger(),
		withServiceWindowClock(func() time.Time { return swNow }),
	)
}

func swMux(h *VehicleServiceWindowHandler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /api/tesla/vehicles/{vehicleId}/service-window", h.ServeHTTP)
	return mux
}

func swPath() string {
	return "/api/tesla/vehicles/" + fixtureSnapshotRowID + "/service-window"
}

func doServiceWindow(h *VehicleServiceWindowHandler, method, path, authHeader, body string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequestWithContext(context.Background(), method, path, nil)
	} else {
		r = httptest.NewRequestWithContext(context.Background(), method, path, strings.NewReader(body))
	}
	if authHeader != "" {
		r.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	swMux(h).ServeHTTP(rec, r)
	return rec
}

// TestVehicleServiceWindowHandler_Validation is the body/validation matrix.
//
// The four CLEAR spellings are grouped deliberately: an owner retracting an
// estimate should not have to guess which one we wanted, so absent key,
// explicit null, empty string and empty body must all behave identically.
func TestVehicleServiceWindowHandler_Validation(t *testing.T) {
	future := swNow.Add(48 * time.Hour)
	futureStr := future.Format(time.RFC3339)

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   wserrors.ErrorCode
		wantWrite  bool
		wantAt     *time.Time
	}{
		{
			name:       "future instant is stored",
			body:       fmt.Sprintf(`{"expectedEndAt":%q}`, futureStr),
			wantStatus: http.StatusOK,
			wantWrite:  true,
			wantAt:     &future,
		},
		{
			name:       "non-UTC future instant is normalized to UTC",
			body:       `{"expectedEndAt":"2026-07-30T05:00:00-07:00"}`,
			wantStatus: http.StatusOK,
			wantWrite:  true,
			wantAt:     ptrTime(time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)),
		},
		{
			name:       "explicit null clears",
			body:       `{"expectedEndAt":null}`,
			wantStatus: http.StatusOK,
			wantWrite:  true,
		},
		{
			name:       "absent key clears",
			body:       `{}`,
			wantStatus: http.StatusOK,
			wantWrite:  true,
		},
		{
			name:       "empty string clears",
			body:       `{"expectedEndAt":""}`,
			wantStatus: http.StatusOK,
			wantWrite:  true,
		},
		{
			name:       "whitespace-only string clears",
			body:       `{"expectedEndAt":"   "}`,
			wantStatus: http.StatusOK,
			wantWrite:  true,
		},
		{
			name:       "empty body clears",
			body:       "",
			wantStatus: http.StatusOK,
			wantWrite:  true,
		},
		{
			// A past instant would emit a window claiming the car is already
			// back, which is worse than emitting nothing: the scheduler bound
			// would then permit rides it should refuse.
			name:       "past instant is rejected",
			body:       fmt.Sprintf(`{"expectedEndAt":%q}`, swNow.Add(-time.Hour).Format(time.RFC3339)),
			wantStatus: http.StatusBadRequest,
			wantCode:   wserrors.ErrCodeInvalidRequest,
		},
		{
			name:       "now is rejected — the bound is strictly future",
			body:       fmt.Sprintf(`{"expectedEndAt":%q}`, swNow.Format(time.RFC3339)),
			wantStatus: http.StatusBadRequest,
			wantCode:   wserrors.ErrCodeInvalidRequest,
		},
		{
			name:       "non-RFC3339 timestamp is rejected",
			body:       `{"expectedEndAt":"next Tuesday"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   wserrors.ErrCodeInvalidRequest,
		},
		{
			name:       "date without time is rejected",
			body:       `{"expectedEndAt":"2026-08-01"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   wserrors.ErrCodeInvalidRequest,
		},
		{
			name:       "malformed JSON is rejected",
			body:       `{"expectedEndAt":`,
			wantStatus: http.StatusBadRequest,
			wantCode:   wserrors.ErrCodeInvalidRequest,
		},
		{
			// Strict decode: a typo'd key must fail loudly rather than silently
			// clearing the owner's estimate.
			name:       "unknown field is rejected",
			body:       `{"expectedEnd":"2026-08-01T15:00:00Z"}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   wserrors.ErrCodeInvalidRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer := &fakeOwnerServiceWindowWriter{}
			h := newServiceWindowHandler(
				&stubVehicleSnapshotReader{row: fixtureSnapshotRow(swUserID)},
				writer,
				swUserID,
			)

			rec := doServiceWindow(h, http.MethodPut, swPath(), swAuthOK, tt.body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}

			if tt.wantStatus != http.StatusOK {
				var env wserrors.ErrorEnvelope
				if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
					t.Fatalf("decode error envelope: %v", err)
				}
				if env.Error.Code != tt.wantCode {
					t.Fatalf("code = %q want %q", env.Error.Code, tt.wantCode)
				}
				if writer.calls != 0 {
					t.Fatalf("a rejected request wrote %d times", writer.calls)
				}
				return
			}

			if tt.wantWrite && writer.calls != 1 {
				t.Fatalf("writer calls = %d want 1", writer.calls)
			}
			switch {
			case tt.wantAt == nil && writer.lastAt != nil:
				t.Fatalf("wrote %v, want nil (clear)", writer.lastAt)
			case tt.wantAt != nil && writer.lastAt == nil:
				t.Fatalf("wrote nil, want %v", tt.wantAt)
			case tt.wantAt != nil && !writer.lastAt.Equal(*tt.wantAt):
				t.Fatalf("wrote %v, want %v", writer.lastAt, tt.wantAt)
			}

			// The response echoes the stored OWNER value, so a client can adopt
			// it without a re-read (there is no WS delta for this field).
			var body vehicleServiceWindowResponse
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body.VehicleID != fixtureSnapshotRowID {
				t.Errorf("vehicleId = %q want %q", body.VehicleID, fixtureSnapshotRowID)
			}
			switch {
			case tt.wantAt == nil && body.ExpectedEndAt != nil:
				t.Errorf("expectedEndAt = %q want null", *body.ExpectedEndAt)
			case tt.wantAt != nil && body.ExpectedEndAt == nil:
				t.Errorf("expectedEndAt = null want %v", tt.wantAt)
			case tt.wantAt != nil && *body.ExpectedEndAt != tt.wantAt.UTC().Format(time.RFC3339):
				t.Errorf("expectedEndAt = %q want %q", *body.ExpectedEndAt, tt.wantAt.UTC().Format(time.RFC3339))
			}
		})
	}
}

// TestVehicleServiceWindowHandler_AuthAndOwnership pins the auth matrix. The
// ownership check is load-bearing here in a way it is not for the plate
// endpoint: the side table has no userId column, so there is no owner-scoped
// SQL predicate behind it.
func TestVehicleServiceWindowHandler_AuthAndOwnership(t *testing.T) {
	body := fmt.Sprintf(`{"expectedEndAt":%q}`, swNow.Add(48*time.Hour).Format(time.RFC3339))

	tests := []struct {
		name       string
		method     string
		authHeader string
		tokenUser  string
		reader     *stubVehicleSnapshotReader
		wantStatus int
		wantCode   wserrors.ErrorCode
	}{
		{
			name:       "wrong method",
			method:     http.MethodPost,
			authHeader: swAuthOK,
			tokenUser:  swUserID,
			reader:     &stubVehicleSnapshotReader{row: fixtureSnapshotRow(swUserID)},
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:       "missing bearer",
			method:     http.MethodPut,
			authHeader: "",
			tokenUser:  swUserID,
			reader:     &stubVehicleSnapshotReader{row: fixtureSnapshotRow(swUserID)},
			wantStatus: http.StatusUnauthorized,
			wantCode:   wserrors.ErrCodeAuthFailed,
		},
		{
			name:       "unknown vehicle is 404, never leaking existence",
			method:     http.MethodPut,
			authHeader: swAuthOK,
			tokenUser:  swUserID,
			reader:     &stubVehicleSnapshotReader{err: fmt.Errorf("lookup: %w", sdk.ErrNotFound)},
			wantStatus: http.StatusNotFound,
			wantCode:   wserrors.ErrCodeNotFound,
		},
		{
			name:       "someone else's vehicle is 403",
			method:     http.MethodPut,
			authHeader: swAuthOK,
			tokenUser:  swOtherUsr,
			reader:     &stubVehicleSnapshotReader{row: fixtureSnapshotRow(swUserID)},
			wantStatus: http.StatusForbidden,
			wantCode:   wserrors.ErrCodeVehicleNotOwned,
		},
		{
			name:       "lookup failure is 500",
			method:     http.MethodPut,
			authHeader: swAuthOK,
			tokenUser:  swUserID,
			reader:     &stubVehicleSnapshotReader{err: errors.New("db down")},
			wantStatus: http.StatusInternalServerError,
			wantCode:   wserrors.ErrCodeInternalError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writer := &fakeOwnerServiceWindowWriter{}
			h := newServiceWindowHandler(tt.reader, writer, tt.tokenUser)

			rec := doServiceWindow(h, tt.method, swPath(), tt.authHeader, body)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantCode != "" {
				var env wserrors.ErrorEnvelope
				if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
					t.Fatalf("decode error envelope: %v", err)
				}
				if env.Error.Code != tt.wantCode {
					t.Fatalf("code = %q want %q", env.Error.Code, tt.wantCode)
				}
			}
			if writer.calls != 0 {
				t.Fatalf("a rejected request wrote %d times", writer.calls)
			}
		})
	}
}

// A store failure is a 500, not a false 200 — the owner must not believe an
// estimate was recorded when it was not.
func TestVehicleServiceWindowHandler_StoreFailure(t *testing.T) {
	writer := &fakeOwnerServiceWindowWriter{err: errors.New("db down")}
	h := newServiceWindowHandler(
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(swUserID)},
		writer,
		swUserID,
	)

	body := fmt.Sprintf(`{"expectedEndAt":%q}`, swNow.Add(48*time.Hour).Format(time.RFC3339))
	rec := doServiceWindow(h, http.MethodPut, swPath(), swAuthOK, body)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d want 500 (body %s)", rec.Code, rec.Body.String())
	}
	var env wserrors.ErrorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if env.Error.Code != wserrors.ErrCodeInternalError {
		t.Fatalf("code = %q want %q", env.Error.Code, wserrors.ErrCodeInternalError)
	}
}
