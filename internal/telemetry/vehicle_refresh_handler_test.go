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

	"github.com/myrobotaxi/telemetry/internal/commands"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

const (
	refreshUserID   = "usr_refresh_owner"
	refreshOtherUsr = "usr_refresh_stranger"
	refreshAuthOK   = "Bearer good-token"
)

// refreshFixedNow pins every clock in these tests so RFC3339 comparisons are exact.
var refreshFixedNow = time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

// --- Test doubles ---

// fakeVehicleRefresher scripts the VehicleRefresher seam. No live Tesla call can fire.
type fakeVehicleRefresher struct {
	lastStream   time.Time
	fresh        bool
	result       RefreshResult
	err          error
	refreshCalls int
}

func (f *fakeVehicleRefresher) LastStreamAt(_ string) (time.Time, bool) {
	return f.lastStream, f.fresh
}

func (f *fakeVehicleRefresher) Refresh(_ context.Context, _, _ string) (RefreshResult, error) {
	f.refreshCalls++
	if f.err != nil {
		return RefreshResult{}, f.err
	}
	return f.result, nil
}

func newRefreshHandler(reader *stubVehicleSnapshotReader, refresher vehicleRefresher, userID string) *VehicleRefreshHandler {
	return NewVehicleRefreshHandler(
		&stubTokenValidator{userID: userID},
		reader,
		refresher,
		discardLogger(),
	)
}

// refreshMux mirrors the production route so r.PathValue("vehicleId") resolves.
func refreshMux(h *VehicleRefreshHandler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/tesla/vehicles/{vehicleId}/refresh", h.ServeHTTP)
	return mux
}

func doRefresh(h *VehicleRefreshHandler, method, path, authHeader string) *httptest.ResponseRecorder {
	req := httptest.NewRequestWithContext(context.Background(), method, path, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rec := httptest.NewRecorder()
	refreshMux(h).ServeHTTP(rec, req)
	return rec
}

func refreshPath() string {
	return "/api/tesla/vehicles/" + fixtureSnapshotRowID + "/refresh"
}

func decodeRefreshBody(t *testing.T, rec *httptest.ResponseRecorder) vehicleRefreshResponse {
	t.Helper()
	var body vehicleRefreshResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return body
}

func assertRefreshErr(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantCode wserrors.ErrorCode) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d want %d (body %s)", rec.Code, wantStatus, rec.Body.String())
	}
	var env wserrors.ErrorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if env.Error.Code != wantCode {
		t.Fatalf("code = %q want %q", env.Error.Code, wantCode)
	}
}

// --- Fresh short-circuit ---

// A vehicle whose stream is inside the MYR-300 freshness window must answer
// `fresh` from local state alone: no wake, no Tesla read, and no cooldown
// consumed (a working car must never be rate-limited for being healthy).
func TestVehicleRefreshHandler_FreshShortCircuit(t *testing.T) {
	streamedAt := refreshFixedNow.Add(-30 * time.Second)
	refresher := &fakeVehicleRefresher{lastStream: streamedAt, fresh: true}
	h := newRefreshHandler(
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(refreshUserID)},
		refresher,
		refreshUserID,
	)

	// Repeat well past the burst-1 cooldown: a fresh answer is unlimited.
	for i := range 3 {
		rec := doRefresh(h, http.MethodPost, refreshPath(), refreshAuthOK)
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d: status = %d want 200 (body %s)", i, rec.Code, rec.Body.String())
		}
		body := decodeRefreshBody(t, rec)
		if body.Status != RefreshStatusFresh {
			t.Fatalf("call %d: status = %q want %q", i, body.Status, RefreshStatusFresh)
		}
		if want := streamedAt.Format(time.RFC3339); body.LastUpdated != want {
			t.Fatalf("call %d: lastUpdated = %q want %q", i, body.LastUpdated, want)
		}
	}

	if refresher.refreshCalls != 0 {
		t.Fatalf("Refresh called %d times — a fresh stream must never dial Tesla", refresher.refreshCalls)
	}
}

// --- Wake + read happy path ---

func TestVehicleRefreshHandler_RefreshedHappyPath(t *testing.T) {
	refresher := &fakeVehicleRefresher{
		fresh:  false,
		result: RefreshResult{Status: RefreshStatusRefreshed, LastUpdated: refreshFixedNow},
	}
	h := newRefreshHandler(
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(refreshUserID)},
		refresher,
		refreshUserID,
	)

	rec := doRefresh(h, http.MethodPost, refreshPath(), refreshAuthOK)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeRefreshBody(t, rec)
	if body.Status != RefreshStatusRefreshed {
		t.Fatalf("status = %q want %q", body.Status, RefreshStatusRefreshed)
	}
	if want := refreshFixedNow.Format(time.RFC3339); body.LastUpdated != want {
		t.Fatalf("lastUpdated = %q want %q", body.LastUpdated, want)
	}
	if refresher.refreshCalls != 1 {
		t.Fatalf("Refresh calls = %d want 1", refresher.refreshCalls)
	}
}

// --- Cooldown ---

// The second Tesla-hitting refresh of the same vehicle inside the window is a
// 429 with Retry-After, and must NOT reach the refresher.
func TestVehicleRefreshHandler_CooldownRateLimits(t *testing.T) {
	refresher := &fakeVehicleRefresher{
		fresh:  false,
		result: RefreshResult{Status: RefreshStatusRefreshed, LastUpdated: refreshFixedNow},
	}
	h := newRefreshHandler(
		&stubVehicleSnapshotReader{row: fixtureSnapshotRow(refreshUserID)},
		refresher,
		refreshUserID,
	)

	if rec := doRefresh(h, http.MethodPost, refreshPath(), refreshAuthOK); rec.Code != http.StatusOK {
		t.Fatalf("first call: status = %d want 200", rec.Code)
	}

	rec := doRefresh(h, http.MethodPost, refreshPath(), refreshAuthOK)
	assertRefreshErr(t, rec, http.StatusTooManyRequests, wserrors.ErrCodeRateLimited)
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("Retry-After = %q want %q", got, "60")
	}
	if refresher.refreshCalls != 1 {
		t.Fatalf("Refresh calls = %d want 1 — the rate-limited call must not dial Tesla", refresher.refreshCalls)
	}
}

// --- Upstream error translation ---

func TestVehicleRefreshHandler_ErrorTranslation(t *testing.T) {
	// asleepErr is what the bounded wake budget produces once spent. It is
	// built through the real Executor so the test pins the ACTUAL typed error
	// the wake path returns, not a hand-rolled look-alike.
	asleepErr := exhaustedWakeError(t)

	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   wserrors.ErrorCode
	}{
		{
			name:       "wake budget exhausted",
			err:        asleepErr,
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   wserrors.ErrCodeVehicleAsleep,
		},
		{
			name:       "car slept between wake and read",
			err:        fmt.Errorf("%w: %w", ErrVehicleDataRead, &FleetAPIError{StatusCode: http.StatusRequestTimeout, Body: "vehicle unavailable"}),
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   wserrors.ErrCodeVehicleAsleep,
		},
		{
			name:       "upstream read failure",
			err:        fmt.Errorf("%w: %w", ErrVehicleDataRead, &FleetAPIError{StatusCode: http.StatusBadGateway, Body: "upstream"}),
			wantStatus: http.StatusBadGateway,
			wantCode:   wserrors.ErrCodeCommandFailed,
		},
		{
			name:       "tesla account not linked",
			err:        fmt.Errorf("resolve tesla token: %w: %w", ErrTeslaTokenUnavailable, errors.New("no row")),
			wantStatus: http.StatusUnauthorized,
			wantCode:   wserrors.ErrCodeAuthFailed,
		},
		{
			name:       "tesla token expired",
			err:        fmt.Errorf("resolve tesla token: %w", ErrTeslaTokenExpired),
			wantStatus: http.StatusUnauthorized,
			wantCode:   wserrors.ErrCodeAuthFailed,
		},
		{
			name:       "publish failure",
			err:        fmt.Errorf("%w: %w", ErrVehicleDataPublish, errors.New("bus closed")),
			wantStatus: http.StatusBadGateway,
			wantCode:   wserrors.ErrCodeCommandFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newRefreshHandler(
				&stubVehicleSnapshotReader{row: fixtureSnapshotRow(refreshUserID)},
				&fakeVehicleRefresher{fresh: false, err: tt.err},
				refreshUserID,
			)
			rec := doRefresh(h, http.MethodPost, refreshPath(), refreshAuthOK)
			assertRefreshErr(t, rec, tt.wantStatus, tt.wantCode)
		})
	}
}

// exhaustedWakeError drives a real Executor against a permanently-asleep probe
// so the test asserts against the genuine bounded-budget error.
func exhaustedWakeError(t *testing.T) error {
	t.Helper()
	ex := commands.NewExecutor(
		&alwaysAwakeTransport{},
		discardLogger(),
		commands.WithConfig(commands.Config{WakeMaxAttempts: 1, WakeBackoff: time.Millisecond, CounterRetryMax: 1}),
	)
	err := ex.EnsureAwake(context.Background(), "5YJ3E1EA1PF000001", "tok",
		func(context.Context) (bool, error) { return false, nil })
	if err == nil {
		t.Fatal("expected an exhausted-budget error")
	}
	return err
}

// alwaysAwakeTransport is a configured transport whose Wake always succeeds —
// the probe, not the transport, is what keeps the vehicle "asleep".
type alwaysAwakeTransport struct{}

func (alwaysAwakeTransport) Command(context.Context, commands.TransportRequest) (commands.TransportResult, error) {
	return commands.TransportResult{}, nil
}
func (alwaysAwakeTransport) Wake(context.Context, string, string) error { return nil }
func (alwaysAwakeTransport) Enabled() bool                             { return true }
func (alwaysAwakeTransport) RESTEnabled() bool                         { return true }

// --- Auth / ownership matrix ---

func TestVehicleRefreshHandler_AuthAndOwnership(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		authHeader string
		tokenUser  string
		reader     *stubVehicleSnapshotReader
		wantStatus int
		wantCode   wserrors.ErrorCode
	}{
		{
			name:       "wrong method",
			method:     http.MethodGet,
			path:       refreshPath(),
			authHeader: refreshAuthOK,
			tokenUser:  refreshUserID,
			reader:     &stubVehicleSnapshotReader{row: fixtureSnapshotRow(refreshUserID)},
			// The mux only registers POST, so a GET never reaches the handler.
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:       "missing bearer",
			method:     http.MethodPost,
			path:       refreshPath(),
			authHeader: "",
			tokenUser:  refreshUserID,
			reader:     &stubVehicleSnapshotReader{row: fixtureSnapshotRow(refreshUserID)},
			wantStatus: http.StatusUnauthorized,
			wantCode:   wserrors.ErrCodeAuthFailed,
		},
		{
			name:       "unknown vehicle is 404, never leaking existence",
			method:     http.MethodPost,
			path:       refreshPath(),
			authHeader: refreshAuthOK,
			tokenUser:  refreshUserID,
			reader:     &stubVehicleSnapshotReader{err: fmt.Errorf("lookup: %w", sdk.ErrNotFound)},
			wantStatus: http.StatusNotFound,
			wantCode:   wserrors.ErrCodeNotFound,
		},
		{
			name:       "someone else's vehicle is 403",
			method:     http.MethodPost,
			path:       refreshPath(),
			authHeader: refreshAuthOK,
			tokenUser:  refreshOtherUsr,
			reader:     &stubVehicleSnapshotReader{row: fixtureSnapshotRow(refreshUserID)},
			wantStatus: http.StatusForbidden,
			wantCode:   wserrors.ErrCodeVehicleNotOwned,
		},
		{
			name:       "lookup failure is 500",
			method:     http.MethodPost,
			path:       refreshPath(),
			authHeader: refreshAuthOK,
			tokenUser:  refreshUserID,
			reader:     &stubVehicleSnapshotReader{err: errors.New("db down")},
			wantStatus: http.StatusInternalServerError,
			wantCode:   wserrors.ErrCodeInternalError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refresher := &fakeVehicleRefresher{fresh: true, lastStream: refreshFixedNow}
			h := newRefreshHandler(tt.reader, refresher, tt.tokenUser)

			rec := doRefresh(h, tt.method, tt.path, tt.authHeader)
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
			if refresher.refreshCalls != 0 {
				t.Fatalf("Refresh calls = %d — a rejected request must never dial Tesla", refresher.refreshCalls)
			}
		})
	}
}
