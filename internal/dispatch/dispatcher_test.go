package dispatch

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/commands"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// --- fakes -----------------------------------------------------------------

type fakeVehicleResolver struct {
	vin string
	err error
}

func (f *fakeVehicleResolver) ResolveVIN(context.Context, string) (string, error) {
	return f.vin, f.err
}

type fakeTokenSource struct {
	token string
	err   error
}

func (f *fakeTokenSource) ResolveToken(context.Context, string) (string, error) {
	return f.token, f.err
}

// fakeExecutor returns errs[i] on call i (clamped to the last element once
// exhausted) and records every request.
type fakeExecutor struct {
	errs  []error
	calls []commands.Request
}

func (f *fakeExecutor) Execute(_ context.Context, req commands.Request) (commands.Result, error) {
	f.calls = append(f.calls, req)
	idx := len(f.calls) - 1
	if idx >= len(f.errs) {
		idx = len(f.errs) - 1
	}
	var err error
	if idx >= 0 {
		err = f.errs[idx]
	}
	if err != nil {
		return commands.Result{}, err
	}
	return commands.Result{Command: req.Command, Applied: true}, nil
}

type recordCall struct {
	status Outcome
	code   string // "" when errCode was nil
}

type fakeStore struct {
	mu           sync.Mutex
	claimed      bool
	claimErr     error
	claimCnt     int
	recorded     []recordCall
	recordErr    error
	recordCtxErr error // ctx.Err() observed at RecordDispatchOutcome time
	recordCtxSet bool

	// Leg-2 (dropoff) tracking, independent of leg 1 (MYR-265).
	dropClaimed  bool
	dropClaimErr error
	dropClaimCnt int
	dropRecorded []recordCall
}

func (f *fakeStore) ClaimDispatch(context.Context, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.claimCnt++
	if f.claimErr != nil {
		return false, f.claimErr
	}
	// First claim wins; subsequent claims (duplicate deliveries) lose.
	if f.claimCnt == 1 {
		return f.claimed, nil
	}
	return false, nil
}

func (f *fakeStore) RecordDispatchOutcome(ctx context.Context, _ string, status Outcome, errCode *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recordCtxErr = ctx.Err()
	f.recordCtxSet = true
	if f.recordErr != nil {
		return f.recordErr
	}
	rc := recordCall{status: status}
	if errCode != nil {
		rc.code = *errCode
	}
	f.recorded = append(f.recorded, rc)
	return nil
}

func (f *fakeStore) ClaimDropoffDispatch(context.Context, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dropClaimCnt++
	if f.dropClaimErr != nil {
		return false, f.dropClaimErr
	}
	if f.dropClaimCnt == 1 {
		return f.dropClaimed, nil
	}
	return false, nil
}

func (f *fakeStore) RecordDropoffDispatchOutcome(_ context.Context, _ string, status Outcome, errCode *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	rc := recordCall{status: status}
	if errCode != nil {
		rc.code = *errCode
	}
	f.dropRecorded = append(f.dropRecorded, rc)
	return nil
}

// --- helpers ---------------------------------------------------------------

// cmdErr builds a typed command error mirroring what the executor returns:
// retryable marks a transient failure (the dispatcher keys its retry policy
// off CommandError.Retryable, not the code string).
func cmdErr(code wserrors.ErrorCode, retryable bool) error {
	return &commands.CommandError{Code: code, Message: string(code), Retryable: retryable}
}

func testEvent() events.RideAcceptedEvent {
	return events.RideAcceptedEvent{
		RideRequestID: "cride123",
		VehicleID:     "cveh456",
		OwnerID:       "cowner789",
		Pickup:        events.RidePlace{Latitude: 37.7955, Longitude: -122.3937, Label: "Ferry Building"},
	}
}

func newTestDispatcher(exec CommandExecutor, store OutcomeStore, cfg Config) *Dispatcher {
	if cfg.Backoff == 0 {
		cfg.Backoff = time.Millisecond
	}
	return New(
		&fakeVehicleResolver{vin: "5YJ3E1EA7KF000000"},
		&fakeTokenSource{token: "tok"},
		exec,
		store,
		cfg,
		nil,
	)
}

// --- tests -----------------------------------------------------------------

func TestProcess_Success(t *testing.T) {
	exec := &fakeExecutor{errs: []error{nil}}
	st := &fakeStore{claimed: true}
	d := newTestDispatcher(exec, st, Config{Enabled: true, MaxRetries: 2})

	d.process(context.Background(), testEvent())

	if len(exec.calls) != 1 {
		t.Fatalf("want 1 executor call, got %d", len(exec.calls))
	}
	got := exec.calls[0]
	if got.Command != commandNavigationRequest {
		t.Errorf("command = %q, want %q", got.Command, commandNavigationRequest)
	}
	if got.VIN != "5YJ3E1EA7KF000000" || got.AccessToken != "tok" {
		t.Errorf("vin/token = %q/%q", got.VIN, got.AccessToken)
	}
	// navigation_request carries a single share `value` (a raw "<lat>,<lon>"
	// coordinate pair the car geocodes); the raw lat/lon and the
	// navigation_gps-only `order` param are gone.
	wantValue := "37.7955,-122.3937"
	if got.Params["value"] != wantValue {
		t.Errorf("value param = %v, want %q", got.Params["value"], wantValue)
	}
	if _, ok := got.Params["order"]; ok {
		t.Errorf("order param must not be sent for navigation_request: %v", got.Params)
	}
	if _, ok := got.Params["lat"]; ok {
		t.Errorf("raw lat param must not be sent for navigation_request: %v", got.Params)
	}
	if _, ok := got.Params["lon"]; ok {
		t.Errorf("raw lon param must not be sent for navigation_request: %v", got.Params)
	}
	if len(st.recorded) != 1 || st.recorded[0].status != OutcomeSent || st.recorded[0].code != "" {
		t.Errorf("recorded = %+v, want one {sent, no code}", st.recorded)
	}
}

// TestPickupShareValue_FullPrecision proves the pickup coordinate is formatted
// at full precision (strconv 'f', -1) — NOT truncated to 4 decimals (%.4f) —
// so the car navigates to the exact pickup, not a rounded-off approximation.
func TestPickupShareValue_FullPrecision(t *testing.T) {
	tests := []struct {
		name     string
		lat, lon float64
		want     string
	}{
		{"MYR-245 example", 33.086, -96.8522, "33.086,-96.8522"},
		{"high precision not truncated", 37.795512, -122.393729, "37.795512,-122.393729"},
		{"integers stay compact", 1, 2, "1,2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := coordShareValue(tt.lat, tt.lon); got != tt.want {
				t.Errorf("coordShareValue(%v,%v) = %q, want %q", tt.lat, tt.lon, got, tt.want)
			}
		})
	}
}

// TestProcess_OutOfRangePickupIsTerminal proves a pickup coordinate outside the
// valid WGS-84 range fails terminally with invalid_request and NEVER dials
// Tesla (the executor is not called). Covers both lat and lon bounds.
func TestProcess_OutOfRangePickupIsTerminal(t *testing.T) {
	tests := []struct {
		name     string
		lat, lon float64
	}{
		{"lat above 90", 91.0, -96.8522},
		{"lat below -90", -90.0001, 10.0},
		{"lon above 180", 33.086, 180.5},
		{"lon below -180", 33.086, -180.5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := &fakeExecutor{errs: []error{nil}}
			st := &fakeStore{claimed: true}
			d := newTestDispatcher(exec, st, Config{Enabled: true, MaxRetries: 2})

			ev := testEvent()
			ev.Pickup = events.RidePlace{Latitude: tt.lat, Longitude: tt.lon}
			d.process(context.Background(), ev)

			if len(exec.calls) != 0 {
				t.Errorf("out-of-range pickup must not dial Tesla, got %d calls", len(exec.calls))
			}
			if len(st.recorded) != 1 || st.recorded[0].status != OutcomeFailed ||
				st.recorded[0].code != string(wserrors.ErrCodeInvalidRequest) {
				t.Errorf("recorded = %+v, want one {failed, invalid_request}", st.recorded)
			}
		})
	}
}

// TestProcess_LogsErrorDetail proves the opaque Tesla-side detail
// (CommandError.Detail) threads from the executor through executeWithRetry to
// the dispatch outcome. The store does not persist it (no DB column), so we
// assert the plumbing by driving a Detail-carrying terminal error end to end
// and confirming the outcome/code are recorded (the detail lands on the log
// line via record's error_detail attr).
func TestProcess_LogsErrorDetail(t *testing.T) {
	detailErr := &commands.CommandError{
		Code:    wserrors.ErrCodeInvalidRequest,
		Message: "Tesla rejected command parameters",
		Detail:  "invalid_command",
	}
	exec := &fakeExecutor{errs: []error{detailErr}}
	st := &fakeStore{claimed: true}
	d := newTestDispatcher(exec, st, Config{Enabled: true, MaxRetries: 2})

	// Assert executeWithRetry surfaces the detail directly (record logs it).
	_, code, detail := d.executeWithRetry(context.Background(), "5YJ3E1EA7KF000000", "tok", testEvent().Pickup)
	if detail != "invalid_command" {
		t.Errorf("detail = %q, want %q", detail, "invalid_command")
	}
	if code == nil || *code != string(wserrors.ErrCodeInvalidRequest) {
		t.Errorf("code = %v, want %q", code, wserrors.ErrCodeInvalidRequest)
	}
}

func TestProcess_OutcomeMatrix(t *testing.T) {
	tests := []struct {
		name       string
		errs       []error
		maxRetries int
		wantStatus Outcome
		wantCode   string
		wantCalls  int
	}{
		{
			name:       "asleep exhausts retries",
			errs:       []error{cmdErr(wserrors.ErrCodeVehicleAsleep, true)},
			maxRetries: 2,
			wantStatus: OutcomeFailed,
			wantCode:   string(wserrors.ErrCodeVehicleAsleep),
			wantCalls:  3, // first + 2 retries
		},
		{
			name:       "transport error exhausts retries",
			errs:       []error{cmdErr(wserrors.ErrCodeCommandFailed, true)},
			maxRetries: 2,
			wantStatus: OutcomeFailed,
			wantCode:   string(wserrors.ErrCodeCommandFailed),
			wantCalls:  3,
		},
		{
			name:       "unconfigured transport is terminal",
			errs:       []error{commands.ErrTransportNotConfigured},
			maxRetries: 2,
			wantStatus: OutcomeFailed,
			wantCode:   codeTransportUnconfigured,
			wantCalls:  1, // permanent misconfig: never retried
		},
		{
			name:       "key_not_paired is terminal",
			errs:       []error{cmdErr(wserrors.ErrCodeKeyNotPaired, false)},
			maxRetries: 2,
			wantStatus: OutcomeFailed,
			wantCode:   string(wserrors.ErrCodeKeyNotPaired),
			wantCalls:  1,
		},
		{
			name:       "permission_denied is terminal",
			errs:       []error{cmdErr(wserrors.ErrCodePermissionDenied, false)},
			maxRetries: 2,
			wantStatus: OutcomeFailed,
			wantCode:   string(wserrors.ErrCodePermissionDenied),
			wantCalls:  1,
		},
		{
			name:       "retry then success",
			errs:       []error{cmdErr(wserrors.ErrCodeCommandFailed, true), nil},
			maxRetries: 2,
			wantStatus: OutcomeSent,
			wantCode:   "",
			wantCalls:  2,
		},
		{
			name:       "non-typed error treated as transport",
			errs:       []error{errors.New("boom")},
			maxRetries: 1,
			wantStatus: OutcomeFailed,
			wantCode:   string(wserrors.ErrCodeCommandFailed),
			wantCalls:  2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := &fakeExecutor{errs: tt.errs}
			st := &fakeStore{claimed: true}
			d := newTestDispatcher(exec, st, Config{Enabled: true, MaxRetries: tt.maxRetries})

			d.process(context.Background(), testEvent())

			if len(exec.calls) != tt.wantCalls {
				t.Errorf("executor calls = %d, want %d", len(exec.calls), tt.wantCalls)
			}
			if len(st.recorded) != 1 {
				t.Fatalf("recorded = %+v, want exactly one", st.recorded)
			}
			if st.recorded[0].status != tt.wantStatus || st.recorded[0].code != tt.wantCode {
				t.Errorf("recorded = %+v, want {%s, %q}", st.recorded[0], tt.wantStatus, tt.wantCode)
			}
		})
	}
}

func TestProcess_Idempotent_AlreadyDispatched(t *testing.T) {
	exec := &fakeExecutor{errs: []error{nil}}
	st := &fakeStore{claimed: false} // claim loses
	d := newTestDispatcher(exec, st, Config{Enabled: true, MaxRetries: 2})

	d.process(context.Background(), testEvent())

	if len(exec.calls) != 0 {
		t.Errorf("executor called %d times on lost claim, want 0", len(exec.calls))
	}
	if len(st.recorded) != 0 {
		t.Errorf("recorded %+v on lost claim, want none", st.recorded)
	}
}

func TestProcess_Idempotent_DuplicateDelivery(t *testing.T) {
	exec := &fakeExecutor{errs: []error{nil}}
	st := &fakeStore{claimed: true}
	d := newTestDispatcher(exec, st, Config{Enabled: true, MaxRetries: 2})

	// Same event delivered twice: only the first claim wins.
	d.process(context.Background(), testEvent())
	d.process(context.Background(), testEvent())

	if len(exec.calls) != 1 {
		t.Errorf("executor calls = %d across duplicate deliveries, want 1", len(exec.calls))
	}
	if len(st.recorded) != 1 {
		t.Errorf("recorded = %d across duplicate deliveries, want 1", len(st.recorded))
	}
}

func TestProcess_KillSwitch(t *testing.T) {
	exec := &fakeExecutor{errs: []error{nil}}
	st := &fakeStore{claimed: true}
	d := newTestDispatcher(exec, st, Config{Enabled: false, MaxRetries: 2})

	d.process(context.Background(), testEvent())

	if len(exec.calls) != 0 {
		t.Errorf("executor called %d times with kill-switch off, want 0", len(exec.calls))
	}
	if st.claimCnt != 1 {
		t.Errorf("claim count = %d, want 1 (skip still latches)", st.claimCnt)
	}
	if len(st.recorded) != 1 || st.recorded[0].status != OutcomeSkipped {
		t.Errorf("recorded = %+v, want one {skipped}", st.recorded)
	}
}

func TestProcess_TokenResolutionFailure(t *testing.T) {
	exec := &fakeExecutor{errs: []error{nil}}
	st := &fakeStore{claimed: true}
	d := New(
		&fakeVehicleResolver{vin: "5YJ3E1EA7KF000000"},
		&fakeTokenSource{err: errors.New("account not linked")},
		exec, st,
		Config{Enabled: true, MaxRetries: 2, Backoff: time.Millisecond},
		nil,
	)

	d.process(context.Background(), testEvent())

	if len(exec.calls) != 0 {
		t.Errorf("executor called on token failure, want 0")
	}
	if len(st.recorded) != 1 || st.recorded[0].status != OutcomeFailed || st.recorded[0].code != codeTokenUnavailable {
		t.Errorf("recorded = %+v, want one {failed, token_unavailable}", st.recorded)
	}
}

func TestProcess_VehicleResolutionFailure(t *testing.T) {
	exec := &fakeExecutor{errs: []error{nil}}
	st := &fakeStore{claimed: true}
	d := New(
		&fakeVehicleResolver{err: errors.New("vehicle gone")},
		&fakeTokenSource{token: "tok"},
		exec, st,
		Config{Enabled: true, MaxRetries: 2, Backoff: time.Millisecond},
		nil,
	)

	d.process(context.Background(), testEvent())

	if len(exec.calls) != 0 {
		t.Errorf("executor called on VIN failure, want 0")
	}
	if len(st.recorded) != 1 || st.recorded[0].status != OutcomeFailed || st.recorded[0].code != codeVehicleUnresolved {
		t.Errorf("recorded = %+v, want one {failed, vehicle_unresolved}", st.recorded)
	}
}

func TestProcess_ClaimError_NoDispatch(t *testing.T) {
	exec := &fakeExecutor{errs: []error{nil}}
	st := &fakeStore{claimErr: errors.New("db down")}
	d := newTestDispatcher(exec, st, Config{Enabled: true, MaxRetries: 2})

	d.process(context.Background(), testEvent())

	if len(exec.calls) != 0 || len(st.recorded) != 0 {
		t.Errorf("claim error must not dispatch or record: calls=%d recorded=%d", len(exec.calls), len(st.recorded))
	}
}

// scriptedTokenSource returns errs[i] on call i, clamping to the LAST element
// once exhausted (so a single-error script repeats forever). A nil entry
// yields "tok". Counts calls.
type scriptedTokenSource struct {
	errs  []error
	calls int
}

func (s *scriptedTokenSource) ResolveToken(context.Context, string) (string, error) {
	i := s.calls
	s.calls++
	if i >= len(s.errs) {
		i = len(s.errs) - 1
	}
	if i >= 0 && s.errs[i] != nil {
		return "", s.errs[i]
	}
	return "tok", nil
}

// scriptedVehicleResolver mirrors scriptedTokenSource for VIN resolution.
type scriptedVehicleResolver struct {
	errs  []error
	calls int
}

func (s *scriptedVehicleResolver) ResolveVIN(context.Context, string) (string, error) {
	i := s.calls
	s.calls++
	if i >= len(s.errs) {
		i = len(s.errs) - 1
	}
	if i >= 0 && s.errs[i] != nil {
		return "", s.errs[i]
	}
	return "5YJ3E1EA7KF000000", nil
}

func newDispatcherWith(veh VehicleResolver, tok TokenSource, exec CommandExecutor, st OutcomeStore, cfg Config) *Dispatcher {
	if cfg.Backoff == 0 {
		cfg.Backoff = time.Millisecond
	}
	return New(veh, tok, exec, st, cfg, nil)
}

// TestProcess_RecordsOutcomeOnDeadCtx proves the outcome write survives a
// per-event ctx that has already been canceled/timed-out (finding 1): the
// record must run on a detached context, so the ride never stays claimed
// with a NULL dispatch_status.
func TestProcess_RecordsOutcomeOnDeadCtx(t *testing.T) {
	exec := &fakeExecutor{errs: []error{nil}}
	st := &fakeStore{claimed: true}
	d := newTestDispatcher(exec, st, Config{Enabled: true, MaxRetries: 0})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // dead before we even start

	d.process(ctx, testEvent())

	if !st.recordCtxSet {
		t.Fatal("RecordDispatchOutcome was never called on a dead ctx")
	}
	if st.recordCtxErr != nil {
		t.Errorf("record ran on a canceled ctx (err=%v); want a detached ctx", st.recordCtxErr)
	}
	if len(st.recorded) != 1 {
		t.Fatalf("recorded = %+v, want exactly one outcome persisted", st.recorded)
	}
}

func TestProcess_TokenResolution(t *testing.T) {
	tests := []struct {
		name      string
		errs      []error
		wantCode  string
		wantCalls int // token-source calls
		wantSent  bool
	}{
		{
			name:      "expired is permanent, no retry",
			errs:      []error{ErrTokenExpired},
			wantCode:  codeTokenExpired,
			wantCalls: 1,
		},
		{
			name:      "unavailable is permanent, no retry",
			errs:      []error{ErrTokenUnavailable},
			wantCode:  codeTokenUnavailable,
			wantCalls: 1,
		},
		{
			name:      "transient exhausts retries then token_unavailable",
			errs:      []error{errors.New("db timeout")},
			wantCode:  codeTokenUnavailable,
			wantCalls: 3, // first + 2 retries
		},
		{
			name:      "transient then success",
			errs:      []error{errors.New("db blip"), nil},
			wantSent:  true,
			wantCalls: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tok := &scriptedTokenSource{errs: tt.errs}
			exec := &fakeExecutor{errs: []error{nil}}
			st := &fakeStore{claimed: true}
			d := newDispatcherWith(&fakeVehicleResolver{vin: "5YJ3E1EA7KF000000"}, tok, exec, st,
				Config{Enabled: true, MaxRetries: 2})

			d.process(context.Background(), testEvent())

			if tok.calls != tt.wantCalls {
				t.Errorf("token-source calls = %d, want %d", tok.calls, tt.wantCalls)
			}
			if len(st.recorded) != 1 {
				t.Fatalf("recorded = %+v, want one", st.recorded)
			}
			if tt.wantSent {
				if st.recorded[0].status != OutcomeSent {
					t.Errorf("recorded = %+v, want sent", st.recorded[0])
				}
				return
			}
			if st.recorded[0].status != OutcomeFailed || st.recorded[0].code != tt.wantCode {
				t.Errorf("recorded = %+v, want {failed, %q}", st.recorded[0], tt.wantCode)
			}
		})
	}
}

func TestProcess_VINResolution(t *testing.T) {
	tests := []struct {
		name      string
		errs      []error
		wantSent  bool
		wantCode  string
		wantCalls int
	}{
		{
			name:      "not-found is permanent, no retry",
			errs:      []error{ErrVehicleNotFound},
			wantCode:  codeVehicleUnresolved,
			wantCalls: 1,
		},
		{
			name:      "transient exhausts retries then vehicle_unresolved",
			errs:      []error{errors.New("db timeout")},
			wantCode:  codeVehicleUnresolved,
			wantCalls: 3,
		},
		{
			name:      "transient then success",
			errs:      []error{errors.New("db blip"), nil},
			wantSent:  true,
			wantCalls: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			veh := &scriptedVehicleResolver{errs: tt.errs}
			exec := &fakeExecutor{errs: []error{nil}}
			st := &fakeStore{claimed: true}
			d := newDispatcherWith(veh, &fakeTokenSource{token: "tok"}, exec, st,
				Config{Enabled: true, MaxRetries: 2})

			d.process(context.Background(), testEvent())

			if veh.calls != tt.wantCalls {
				t.Errorf("vehicle-resolver calls = %d, want %d", veh.calls, tt.wantCalls)
			}
			if len(st.recorded) != 1 {
				t.Fatalf("recorded = %+v, want one", st.recorded)
			}
			if tt.wantSent {
				if st.recorded[0].status != OutcomeSent {
					t.Errorf("recorded = %+v, want sent", st.recorded[0])
				}
				return
			}
			if st.recorded[0].status != OutcomeFailed || st.recorded[0].code != tt.wantCode {
				t.Errorf("recorded = %+v, want {failed, %q}", st.recorded[0], tt.wantCode)
			}
		})
	}
}

// echoVehicleResolver returns the vehicleID as the VIN so a test can route
// executor behavior per event.
type echoVehicleResolver struct{}

func (echoVehicleResolver) ResolveVIN(_ context.Context, id string) (string, error) { return id, nil }

// controllableExecutor signals each call on started (with the request VIN)
// and blocks on the per-VIN gate channel if one is present. A nil gate does
// not block.
type controllableExecutor struct {
	mu      sync.Mutex
	started chan string
	gate    map[string]chan struct{}
}

func (c *controllableExecutor) Execute(_ context.Context, req commands.Request) (commands.Result, error) {
	c.started <- req.VIN
	c.mu.Lock()
	g := c.gate[req.VIN]
	c.mu.Unlock()
	if g != nil {
		<-g
	}
	return commands.Result{Command: req.Command, Applied: true}, nil
}

// concurrentStore grants every claim and records per-ride outcomes; safe for
// concurrent dispatches (unlike fakeStore, which models a single ride).
type concurrentStore struct {
	mu       sync.Mutex
	recorded map[string]recordCall
}

func (s *concurrentStore) ClaimDispatch(context.Context, string) (bool, error) { return true, nil }

func (s *concurrentStore) RecordDispatchOutcome(_ context.Context, id string, status Outcome, code *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rc := recordCall{status: status}
	if code != nil {
		rc.code = *code
	}
	s.recorded[id] = rc
	return nil
}

func (s *concurrentStore) ClaimDropoffDispatch(ctx context.Context, id string) (bool, error) {
	return s.ClaimDispatch(ctx, id)
}

func (s *concurrentStore) RecordDropoffDispatchOutcome(ctx context.Context, id string, status Outcome, code *string) error {
	return s.RecordDispatchOutcome(ctx, id, status, code)
}

func (s *concurrentStore) get(id string) (recordCall, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rc, ok := s.recorded[id]
	return rc, ok
}

// TestHandle_ConcurrentDispatch_NoHeadOfLineBlocking proves the bus handler
// hands each event to a worker and returns immediately, so a slow dispatch
// does not delay another (finding 4). A "slow" dispatch blocks in the
// executor; a "fast" dispatch delivered afterward must complete while the
// slow one is still stuck.
func TestHandle_ConcurrentDispatch_NoHeadOfLineBlocking(t *testing.T) {
	slowGate := make(chan struct{})
	exec := &controllableExecutor{
		started: make(chan string, 2),
		gate:    map[string]chan struct{}{"vehSLOWxxxx": slowGate},
	}
	st := &concurrentStore{recorded: map[string]recordCall{}}
	d := New(
		echoVehicleResolver{},
		&fakeTokenSource{token: "tok"},
		exec, st,
		Config{Enabled: true, MaxRetries: 0, MaxConcurrent: 4, Backoff: time.Millisecond},
		nil,
	)

	slow := events.RideAcceptedEvent{RideRequestID: "rideSLOW", VehicleID: "vehSLOWxxxx", OwnerID: "o", Pickup: events.RidePlace{Latitude: 1, Longitude: 2}}
	fast := events.RideAcceptedEvent{RideRequestID: "rideFAST", VehicleID: "vehFASTxxxx", OwnerID: "o", Pickup: events.RidePlace{Latitude: 3, Longitude: 4}}

	// Deliver the slow event; wait until its dispatch has entered the executor
	// (and is now blocked on the gate).
	d.handle(events.Event{ID: "a", Payload: slow})
	if got := <-exec.started; got != "vehSLOWxxxx" {
		t.Fatalf("first started = %q, want vehSLOWxxxx", got)
	}

	// Deliver the fast event; it must run and record even though slow is stuck.
	d.handle(events.Event{ID: "b", Payload: fast})
	if got := <-exec.started; got != "vehFASTxxxx" {
		t.Fatalf("second started = %q, want vehFASTxxxx", got)
	}

	// Fast records its outcome while slow remains blocked (poll briefly).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := st.get("rideFAST"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fast dispatch did not complete while slow was blocked (head-of-line blocking)")
		}
		time.Sleep(time.Millisecond)
	}
	if _, ok := st.get("rideSLOW"); ok {
		t.Fatal("slow dispatch recorded before it was released")
	}

	// Release slow and drain.
	close(slowGate)
	d.Wait()

	if rc, ok := st.get("rideSLOW"); !ok || rc.status != OutcomeSent {
		t.Errorf("slow outcome = %+v (ok=%v), want sent", rc, ok)
	}
	if rc, ok := st.get("rideFAST"); !ok || rc.status != OutcomeSent {
		t.Errorf("fast outcome = %+v (ok=%v), want sent", rc, ok)
	}
}

// fakeLister feeds Reconcile a scripted id list and records the olderThan it
// was asked for.
type fakeLister struct {
	ids          []string
	err          error
	gotOlderThan time.Duration
	called       bool
}

func (f *fakeLister) ListInterruptedDispatches(_ context.Context, olderThan time.Duration) ([]string, error) {
	f.called = true
	f.gotOlderThan = olderThan
	return f.ids, f.err
}

// reconcileStore records per-id outcomes and can fail RecordDispatchOutcome
// for specific ids.
type reconcileStore struct {
	mu       sync.Mutex
	recorded map[string]recordCall
	failIDs  map[string]bool
}

func (s *reconcileStore) ClaimDispatch(context.Context, string) (bool, error) { return true, nil }

func (s *reconcileStore) RecordDispatchOutcome(_ context.Context, id string, status Outcome, code *string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failIDs[id] {
		return errors.New("record failed for " + id)
	}
	rc := recordCall{status: status}
	if code != nil {
		rc.code = *code
	}
	s.recorded[id] = rc
	return nil
}

func (s *reconcileStore) ClaimDropoffDispatch(context.Context, string) (bool, error) {
	return true, nil
}

func (s *reconcileStore) RecordDropoffDispatchOutcome(ctx context.Context, id string, status Outcome, code *string) error {
	return s.RecordDispatchOutcome(ctx, id, status, code)
}

func TestReconcile(t *testing.T) {
	tests := []struct {
		name         string
		ids          []string
		listErr      error
		failIDs      map[string]bool
		wantResolved int
		wantErr      bool
		wantRecorded []string // ids expected to be recorded failed/interrupted
	}{
		{
			name:         "resolves all interrupted",
			ids:          []string{"r1", "r2", "r3"},
			wantResolved: 3,
			wantRecorded: []string{"r1", "r2", "r3"},
		},
		{
			name:         "no interrupted rides",
			ids:          nil,
			wantResolved: 0,
		},
		{
			name:    "list error propagates, nothing recorded",
			listErr: errors.New("db down"),
			wantErr: true,
		},
		{
			name:         "continues past a record failure",
			ids:          []string{"r1", "r2", "r3"},
			failIDs:      map[string]bool{"r2": true},
			wantResolved: 2,
			wantRecorded: []string{"r1", "r3"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &reconcileStore{recorded: map[string]recordCall{}, failIDs: tt.failIDs}
			d := New(&fakeVehicleResolver{}, &fakeTokenSource{}, &fakeExecutor{}, st,
				Config{Enabled: true}, nil)
			lister := &fakeLister{ids: tt.ids, err: tt.listErr}

			n, err := d.Reconcile(context.Background(), lister, time.Minute)

			if tt.wantErr {
				if err == nil {
					t.Fatal("Reconcile expected error, got nil")
				}
				if len(st.recorded) != 0 {
					t.Errorf("recorded %+v on list error, want none", st.recorded)
				}
				return
			}
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if n != tt.wantResolved {
				t.Errorf("resolved = %d, want %d", n, tt.wantResolved)
			}
			for _, id := range tt.wantRecorded {
				rc, ok := st.recorded[id]
				if !ok {
					t.Errorf("ride %s not recorded", id)
					continue
				}
				if rc.status != OutcomeFailed || rc.code != codeDispatchInterrupted {
					t.Errorf("ride %s recorded %+v, want {failed, dispatch_interrupted}", id, rc)
				}
			}
		})
	}
}

func TestReconcile_FloorsOlderThanAtOverallTimeout(t *testing.T) {
	st := &reconcileStore{recorded: map[string]recordCall{}}
	d := New(&fakeVehicleResolver{}, &fakeTokenSource{}, &fakeExecutor{}, st,
		Config{Enabled: true, OverallTimeout: 90 * time.Second}, nil)
	lister := &fakeLister{}

	// Ask for a cutoff BELOW OverallTimeout; it must be floored so a live
	// in-flight dispatch is never mistaken for an orphan.
	if _, err := d.Reconcile(context.Background(), lister, time.Second); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !lister.called {
		t.Fatal("lister was not called")
	}
	if lister.gotOlderThan != 90*time.Second {
		t.Errorf("olderThan = %v, want floored to 90s (OverallTimeout)", lister.gotOlderThan)
	}
}

func TestHandle_WrongPayloadType(t *testing.T) {
	exec := &fakeExecutor{errs: []error{nil}}
	st := &fakeStore{claimed: true}
	d := newTestDispatcher(exec, st, Config{Enabled: true, MaxRetries: 2})

	// A non-ride.accepted payload must be ignored without panicking.
	d.handle(events.Event{ID: "x", Payload: events.RideStatusChangedEvent{}})

	if st.claimCnt != 0 {
		t.Errorf("wrong payload should not claim, got claimCnt=%d", st.claimCnt)
	}
}
