package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
	"github.com/myrobotaxi/telemetry/pkg/sdk"
)

// fakeCompleter stands in for *SetupCompleter so no test can dial Tesla. It
// counts invocations, which is how the cooldown and concurrency guarantees are
// asserted: the claim is not "the handler returned 429", it is "the sequence
// ran exactly once".
type fakeCompleter struct {
	calls atomic.Int64
	state SetupState
	err   error
	// block, when non-nil, holds each call until it is closed — the only way
	// to make two requests genuinely overlap.
	block chan struct{}
}

func (f *fakeCompleter) Complete(_ context.Context, _ VehicleSnapshotRow) (SetupState, error) {
	f.calls.Add(1)
	if f.block != nil {
		<-f.block
	}
	return f.state, f.err
}

// newCompletionHandler wires a handler for the owner of the seeded rows.
func newCompletionHandler(reader *fakeSnapshotReader, completer setupCompleter) *SetupCompletionHandler {
	return NewSetupCompletionHandler(
		&stubTokenValidator{userID: "user-1"}, reader, completer, discardLogger())
}

func completionRequest(vehicleID string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), http.MethodPost,
		"/api/tesla/vehicles/"+vehicleID+"/complete-setup", nil)
	r.SetPathValue("vehicleId", vehicleID)
	r.Header.Set("Authorization", "Bearer session-jwt")
	return r
}

// okCompleter returns a handler wired to a car that completes successfully.
func okCompleter() (*SetupCompletionHandler, *fakeCompleter) {
	c := &fakeCompleter{state: SetupState{State: SetupStateConfiguring, Since: "2026-08-09T04:00:00Z"}}
	return newCompletionHandler(&fakeSnapshotReader{rows: []VehicleSnapshotRow{setupRow(nil)}}, c), c
}

// TestCompleteSetupHandlerAuthorization pins the §7.12/§7.14/§7.15 convention:
// an unknown vehicle and an unowned one must be indistinguishable in the
// direction that matters (existence is never leaked), and neither may reach the
// completion sequence at all.
func TestCompleteSetupHandlerAuthorization(t *testing.T) {
	tests := []struct {
		name       string
		vehicleID  string
		method     string
		noAuth     bool
		authErr    error
		readerErr  error
		callerID   string
		wantStatus int
		wantCode   wserrors.ErrorCode
	}{
		{
			name: "owner succeeds", vehicleID: "veh-1", callerID: "user-1",
			wantStatus: http.StatusOK,
		},
		{
			name: "missing vehicleId", vehicleID: "", callerID: "user-1",
			wantStatus: http.StatusBadRequest, wantCode: wserrors.ErrCodeInvalidRequest,
		},
		{
			name: "no bearer", vehicleID: "veh-1", noAuth: true, callerID: "user-1",
			wantStatus: http.StatusUnauthorized, wantCode: wserrors.ErrCodeAuthFailed,
		},
		{
			name: "invalid bearer", vehicleID: "veh-1", authErr: errors.New("expired"), callerID: "user-1",
			wantStatus: http.StatusUnauthorized, wantCode: wserrors.ErrCodeAuthFailed,
		},
		{
			name: "unknown vehicle is 404, never 403", vehicleID: "veh-1",
			readerErr: sdk.ErrNotFound, callerID: "user-1",
			wantStatus: http.StatusNotFound, wantCode: wserrors.ErrCodeNotFound,
		},
		{
			name: "someone else's car", vehicleID: "veh-1", callerID: "intruder",
			wantStatus: http.StatusForbidden, wantCode: wserrors.ErrCodeVehicleNotOwned,
		},
		{
			name: "store failure", vehicleID: "veh-1", readerErr: errors.New("pool exhausted"), callerID: "user-1",
			wantStatus: http.StatusInternalServerError, wantCode: wserrors.ErrCodeInternalError,
		},
		{
			name: "GET is rejected", vehicleID: "veh-1", method: http.MethodGet, callerID: "user-1",
			wantStatus: http.StatusMethodNotAllowed, wantCode: wserrors.ErrCodeInvalidRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := &fakeSnapshotReader{rows: []VehicleSnapshotRow{setupRow(nil)}, err: tt.readerErr}
			completer := &fakeCompleter{state: SetupState{State: SetupStateConfiguring, Since: "2026-08-09T04:00:00Z"}}
			h := NewSetupCompletionHandler(
				&stubTokenValidator{userID: tt.callerID, err: tt.authErr}, reader, completer, discardLogger())

			req := completionRequest(tt.vehicleID)
			if tt.method != "" {
				req.Method = tt.method
			}
			if tt.noAuth {
				req.Header.Del("Authorization")
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.wantCode != "" {
				assertErrorCode(t, rec, tt.wantCode)
				if got := completer.calls.Load(); got != 0 {
					t.Errorf("completion ran %d times on a rejected request; want 0", got)
				}
			}
		})
	}
}

// TestCompleteSetupHandlerResponse pins the success body: the vehicle it is
// about and the one thing the client renders, nothing else.
func TestCompleteSetupHandlerResponse(t *testing.T) {
	h, _ := okCompleter()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, completionRequest("veh-1"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body) != 2 {
		t.Errorf("response keys = %v, want exactly vehicleId + setupState", body)
	}
	if body["vehicleId"] != "veh-1" {
		t.Errorf("vehicleId = %v, want veh-1", body["vehicleId"])
	}
	state, ok := body["setupState"].(map[string]any)
	if !ok {
		t.Fatalf("setupState = %v, want an object", body["setupState"])
	}
	if state["state"] != SetupStateConfiguring || state["since"] == "" {
		t.Errorf("setupState = %v, want {state, since} populated", state)
	}
}

// TestCompleteSetupHandlerCooldown: the second Tesla-hitting call inside the
// window is refused, and — the part that matters — the sequence does not run.
func TestCompleteSetupHandlerCooldown(t *testing.T) {
	h, completer := okCompleter()

	first := httptest.NewRecorder()
	h.ServeHTTP(first, completionRequest("veh-1"))
	if first.Code != http.StatusOK {
		t.Fatalf("first call status = %d, want 200", first.Code)
	}

	second := httptest.NewRecorder()
	h.ServeHTTP(second, completionRequest("veh-1"))
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("second call status = %d, want 429", second.Code)
	}
	assertErrorCode(t, second, wserrors.ErrCodeRateLimited)
	if got := second.Header().Get("Retry-After"); got != "60" {
		t.Errorf("Retry-After = %q, want 60", got)
	}
	if got := completer.calls.Load(); got != 1 {
		t.Errorf("completions = %d, want 1 — the cooldown must stop the WORK, not just the response", got)
	}
}

// TestCompleteSetupHandlerCooldownIsPerVehicle: throttling one car must not
// throttle another owner's onboarding.
func TestCompleteSetupHandlerCooldownIsPerVehicle(t *testing.T) {
	other := setupRow(func(r *VehicleSnapshotRow) {
		r.ID = "veh-2"
		r.VIN = "7SAYGDED5TA999274"
	})
	reader := &fakeSnapshotReader{rows: []VehicleSnapshotRow{setupRow(nil), other}}
	completer := &fakeCompleter{state: SetupState{State: SetupStateConfiguring, Since: "2026-08-09T04:00:00Z"}}
	h := newCompletionHandler(reader, completer)

	for _, id := range []string{"veh-1", "veh-2"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, completionRequest(id))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", id, rec.Code)
		}
	}
	if got := completer.calls.Load(); got != 2 {
		t.Errorf("completions = %d, want 2 (the limiter is keyed per vehicle)", got)
	}
}

// TestCompleteSetupHandlerConcurrentCallsRunOnce is the race proof. Twenty
// simultaneous requests for one car — a client whose retry loop has come
// unstuck — must produce exactly ONE run of the sequence, so exactly one wake
// and at most one delete-then-create reach the vehicle.
//
// The completer BLOCKS, so the calls genuinely overlap rather than merely
// arriving close together; the cooldown is longer than the probe budget
// precisely so this cannot interleave.
func TestCompleteSetupHandlerConcurrentCallsRunOnce(t *testing.T) {
	completer := &fakeCompleter{
		state: SetupState{State: SetupStateConfiguring, Since: "2026-08-09T04:00:00Z"},
		block: make(chan struct{}),
	}
	h := newCompletionHandler(
		&fakeSnapshotReader{rows: []VehicleSnapshotRow{setupRow(nil)}}, completer)

	const callers = 20
	var wg sync.WaitGroup
	codes := make([]int, callers)
	start := make(chan struct{})

	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, completionRequest("veh-1"))
			codes[i] = rec.Code
		}()
	}
	close(start)

	// Let the winner reach the blocking completer, then release it.
	deadline := time.After(2 * time.Second)
	for completer.calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("no request ever reached the completion sequence")
		default:
		}
	}
	close(completer.block)
	wg.Wait()

	if got := completer.calls.Load(); got != 1 {
		t.Fatalf("completions = %d, want exactly 1 across %d concurrent callers", got, callers)
	}
	ok, throttled := 0, 0
	for _, c := range codes {
		switch c {
		case http.StatusOK:
			ok++
		case http.StatusTooManyRequests:
			throttled++
		default:
			t.Errorf("unexpected status %d", c)
		}
	}
	if ok != 1 || throttled != callers-1 {
		t.Errorf("got %d OK / %d throttled, want 1 / %d", ok, throttled, callers-1)
	}
}

// TestCompleteSetupHandlerErrorMapping: every sentinel maps to a typed code,
// and NONE of them carries a setupState — a client must never render a setup
// card from a request that could not honestly claim one.
func TestCompleteSetupHandlerErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   wserrors.ErrorCode
	}{
		{
			name: "paired but unreachable is transient", err: ErrSetupVehicleUnreachable,
			wantStatus: http.StatusServiceUnavailable, wantCode: wserrors.ErrCodeVehicleAsleep,
		},
		{
			name: "no signing proxy is a service fact", err: ErrSetupPushUnavailable,
			wantStatus: http.StatusServiceUnavailable, wantCode: wserrors.ErrCodeServiceUnavailable,
		},
		{
			name: "unreadable pairing probe", err: fmt.Errorf("%w: 429", ErrSetupProbeUnavailable),
			wantStatus: http.StatusBadGateway, wantCode: wserrors.ErrCodeCommandFailed,
		},
		{
			name: "failed push never claims configuring", err: ErrSetupPushFailed,
			wantStatus: http.StatusBadGateway, wantCode: wserrors.ErrCodeCommandFailed,
		},
		{
			name: "anything else", err: errors.New("boom"),
			wantStatus: http.StatusBadGateway, wantCode: wserrors.ErrCodeCommandFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			completer := &fakeCompleter{err: tt.err}
			h := newCompletionHandler(
				&fakeSnapshotReader{rows: []VehicleSnapshotRow{setupRow(nil)}}, completer)

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, completionRequest("veh-1"))

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
			assertErrorCode(t, rec, tt.wantCode)

			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if _, present := body["setupState"]; present {
				t.Error("an error response carried a setupState; the client must not render a card from it")
			}
		})
	}
}

// assertErrorCode reads the §4.1 typed error envelope.
func assertErrorCode(t *testing.T, rec *httptest.ResponseRecorder, want wserrors.ErrorCode) {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v (body %s)", err, rec.Body.String())
	}
	if env.Error.Code != string(want) {
		t.Errorf("error.code = %q, want %q", env.Error.Code, want)
	}
}
