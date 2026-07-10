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
	mu        sync.Mutex
	claimed   bool
	claimErr  error
	claimCnt  int
	recorded  []recordCall
	recordErr error
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

func (f *fakeStore) RecordDispatchOutcome(_ context.Context, _ string, status Outcome, errCode *string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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

// --- helpers ---------------------------------------------------------------

func cmdErr(code wserrors.ErrorCode) error {
	return &commands.CommandError{Code: code, Message: string(code)}
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
	if got.Command != commandNavigationGPS {
		t.Errorf("command = %q, want %q", got.Command, commandNavigationGPS)
	}
	if got.VIN != "5YJ3E1EA7KF000000" || got.AccessToken != "tok" {
		t.Errorf("vin/token = %q/%q", got.VIN, got.AccessToken)
	}
	if got.Params["lat"] != 37.7955 || got.Params["lon"] != -122.3937 {
		t.Errorf("lat/lon params = %v/%v", got.Params["lat"], got.Params["lon"])
	}
	if got.Params["order"] != float64(0) {
		t.Errorf("order param = %v, want float64(0)", got.Params["order"])
	}
	if len(st.recorded) != 1 || st.recorded[0].status != OutcomeSent || st.recorded[0].code != "" {
		t.Errorf("recorded = %+v, want one {sent, no code}", st.recorded)
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
			errs:       []error{cmdErr(wserrors.ErrCodeVehicleAsleep)},
			maxRetries: 2,
			wantStatus: OutcomeFailed,
			wantCode:   string(wserrors.ErrCodeVehicleAsleep),
			wantCalls:  3, // first + 2 retries
		},
		{
			name:       "transport error exhausts retries",
			errs:       []error{cmdErr(wserrors.ErrCodeCommandFailed)},
			maxRetries: 2,
			wantStatus: OutcomeFailed,
			wantCode:   string(wserrors.ErrCodeCommandFailed),
			wantCalls:  3,
		},
		{
			name:       "key_not_paired is terminal",
			errs:       []error{cmdErr(wserrors.ErrCodeKeyNotPaired)},
			maxRetries: 2,
			wantStatus: OutcomeFailed,
			wantCode:   string(wserrors.ErrCodeKeyNotPaired),
			wantCalls:  1,
		},
		{
			name:       "permission_denied is terminal",
			errs:       []error{cmdErr(wserrors.ErrCodePermissionDenied)},
			maxRetries: 2,
			wantStatus: OutcomeFailed,
			wantCode:   string(wserrors.ErrCodePermissionDenied),
			wantCalls:  1,
		},
		{
			name:       "retry then success",
			errs:       []error{cmdErr(wserrors.ErrCodeCommandFailed), nil},
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
