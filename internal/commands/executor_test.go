package commands

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// fakeTransport is a scripted Transport: it returns the next queued result
// on each Command call and records how it was used. No live Tesla calls.
type fakeTransport struct {
	enabled bool
	results []TransportResult
	cmdErr  error
	calls   []TransportRequest
	wakes   int
	wakeErr error
}

func (f *fakeTransport) Enabled() bool { return f.enabled }

// RESTEnabled mirrors enabled: the fake models a transport that is either fully
// configured (both signing + REST) or fully unconfigured, which is all the
// executor gate needs to exercise both the signed and unsigned degraded paths.
func (f *fakeTransport) RESTEnabled() bool { return f.enabled }

func (f *fakeTransport) Command(_ context.Context, req TransportRequest) (TransportResult, error) {
	f.calls = append(f.calls, req)
	if f.cmdErr != nil {
		return TransportResult{}, f.cmdErr
	}
	idx := len(f.calls) - 1
	if idx >= len(f.results) {
		idx = len(f.results) - 1 // repeat the last scripted result
	}
	return f.results[idx], nil
}

func (f *fakeTransport) Wake(_ context.Context, _, _ string) error {
	f.wakes++
	return f.wakeErr
}

// grantAll is a scope set that grants both command scopes.
func grantAll() ScopeSet {
	return ScopeSet{known: true, set: map[Scope]struct{}{
		ScopeVehicleCmds:  {},
		ScopeChargingCmds: {},
	}}
}

// fastConfig shrinks the wake backoff so tests don't sleep for seconds.
func fastConfig() Config {
	return Config{WakeMaxAttempts: 3, WakeBackoff: time.Millisecond, CounterRetryMax: 1}
}

func asCommandError(err error, target **CommandError) bool {
	return errors.As(err, target)
}

func wantCode(t *testing.T, err error, code wserrors.ErrorCode) {
	t.Helper()
	var cErr *CommandError
	if !asCommandError(err, &cErr) {
		t.Fatalf("error is not *CommandError: %v", err)
	}
	if cErr.Code != code {
		t.Fatalf("code = %q want %q (msg: %s)", cErr.Code, code, cErr.Message)
	}
}

func newExec(tr Transport) *Executor {
	return NewExecutor(tr, nil, WithConfig(fastConfig()))
}

func TestExecute_UnknownCommand(t *testing.T) {
	e := newExec(&fakeTransport{enabled: true})
	_, err := e.Execute(context.Background(), Request{VIN: "V", Command: "does_not_exist", Scopes: grantAll()})
	wantCode(t, err, wserrors.ErrCodeInvalidRequest)
}

func TestExecute_ScopeGatingDeniesWithoutTransport(t *testing.T) {
	tr := &fakeTransport{enabled: true, results: []TransportResult{{Outcome: OutcomeOK}}}
	e := newExec(tr)
	// Token known to grant only charging scope; door_lock needs vehicle_cmds.
	scopes := ScopeSet{known: true, set: map[Scope]struct{}{ScopeChargingCmds: {}}}
	_, err := e.Execute(context.Background(), Request{VIN: "V", Command: "door_lock", Scopes: scopes})
	wantCode(t, err, wserrors.ErrCodePermissionDenied)
	if len(tr.calls) != 0 {
		t.Fatalf("scope denial must not call the transport (calls=%d)", len(tr.calls))
	}
}

func TestExecute_UnknownScopesDefersToTesla(t *testing.T) {
	tr := &fakeTransport{enabled: true, results: []TransportResult{{Outcome: OutcomeOK}}}
	e := newExec(tr)
	// Scopes not Known -> executor proceeds and lets Tesla enforce.
	res, err := e.Execute(context.Background(), Request{VIN: "V", Command: "door_lock", Scopes: ScopeSet{}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Applied || len(tr.calls) != 1 {
		t.Fatalf("expected one transport call and applied result")
	}
}

func TestExecute_KeyNotPairedWhenTransportDisabled(t *testing.T) {
	tr := &fakeTransport{enabled: false}
	e := newExec(tr)
	_, err := e.Execute(context.Background(), Request{VIN: "V", Command: "door_lock", Scopes: grantAll()})
	wantCode(t, err, wserrors.ErrCodeKeyNotPaired)
	if len(tr.calls) != 0 {
		t.Fatalf("disabled transport must not be dialed")
	}
}

func TestExecute_NavigationFailsWhenTransportDisabled(t *testing.T) {
	// navigation_request is unsigned, but without a proxy it still cannot
	// reach the vehicle -> command_failed (not key_not_paired).
	tr := &fakeTransport{enabled: false}
	e := newExec(tr)
	_, err := e.Execute(context.Background(), Request{
		VIN: "V", Command: "navigation_request",
		Params: map[string]any{"value": "somewhere"}, Scopes: grantAll(),
	})
	wantCode(t, err, wserrors.ErrCodeCommandFailed)

	// The unconfigured-transport failure is PERMANENT: it carries the
	// ErrTransportNotConfigured sentinel and is non-retryable, so an async
	// caller can short-circuit its retry budget (MYR-176 finding 7).
	if !errors.Is(err, ErrTransportNotConfigured) {
		t.Errorf("err = %v, want ErrTransportNotConfigured sentinel", err)
	}
	var cErr *CommandError
	if asCommandError(err, &cErr) && cErr.Retryable {
		t.Error("unconfigured-transport failure must not be retryable")
	}
}

func TestExecute_TransportErrorIsRetryable(t *testing.T) {
	// A transient transport error (proxy dialed but the call failed) IS
	// retryable, distinct from the permanent unconfigured case above.
	tr := &fakeTransport{enabled: true, cmdErr: errors.New("dial tcp: connection refused")}
	e := newExec(tr)
	_, err := e.Execute(context.Background(), Request{
		VIN: "V", Command: "navigation_gps_request",
		Params: map[string]any{"lat": 1.0, "lon": 2.0}, Scopes: grantAll(),
	})
	wantCode(t, err, wserrors.ErrCodeCommandFailed)
	if errors.Is(err, ErrTransportNotConfigured) {
		t.Error("a live transport error must not carry ErrTransportNotConfigured")
	}
	var cErr *CommandError
	if asCommandError(err, &cErr) && !cErr.Retryable {
		t.Error("a transient transport error must be retryable")
	}
}

func TestExecute_HappyPathForwardsTeslaCommand(t *testing.T) {
	tr := &fakeTransport{enabled: true, results: []TransportResult{{Outcome: OutcomeOK}}}
	e := newExec(tr)
	res, err := e.Execute(context.Background(), Request{
		VIN: "5YJ", Command: "set_charge_limit",
		Params: map[string]any{"percent": 80.0}, AccessToken: "tok", Scopes: grantAll(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Applied || res.Command != "set_charge_limit" {
		t.Fatalf("result = %+v", res)
	}
	if len(tr.calls) != 1 {
		t.Fatalf("expected one call, got %d", len(tr.calls))
	}
	call := tr.calls[0]
	if call.Command != "set_charge_limit" || call.VIN != "5YJ" || call.Token != "tok" {
		t.Fatalf("transport request = %+v", call)
	}
	if string(call.Body) != `{"percent":80}` {
		t.Fatalf("body = %s", call.Body)
	}
}

func TestExecute_WakeRetryThenSuccess(t *testing.T) {
	tr := &fakeTransport{enabled: true, results: []TransportResult{
		{Outcome: OutcomeAsleep},
		{Outcome: OutcomeAsleep},
		{Outcome: OutcomeOK},
	}}
	e := newExec(tr)
	res, err := e.Execute(context.Background(), Request{VIN: "V", Command: "door_lock", Scopes: grantAll()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Applied {
		t.Fatalf("expected applied after wake retries")
	}
	if tr.wakes != 2 {
		t.Fatalf("expected 2 wake calls, got %d", tr.wakes)
	}
	if len(tr.calls) != 3 {
		t.Fatalf("expected 3 command attempts, got %d", len(tr.calls))
	}
}

func TestExecute_WakeRetryExhausted(t *testing.T) {
	tr := &fakeTransport{enabled: true, results: []TransportResult{{Outcome: OutcomeAsleep}}}
	e := newExec(tr)
	_, err := e.Execute(context.Background(), Request{VIN: "V", Command: "door_lock", Scopes: grantAll()})
	wantCode(t, err, wserrors.ErrCodeVehicleAsleep)
	// Initial attempt + WakeMaxAttempts retries.
	if len(tr.calls) != fastConfig().WakeMaxAttempts+1 {
		t.Fatalf("attempts = %d want %d", len(tr.calls), fastConfig().WakeMaxAttempts+1)
	}
	if tr.wakes != fastConfig().WakeMaxAttempts {
		t.Fatalf("wakes = %d want %d", tr.wakes, fastConfig().WakeMaxAttempts)
	}
}

func TestExecute_WakeRetrySurvivesWakeError(t *testing.T) {
	tr := &fakeTransport{
		enabled: true,
		wakeErr: errors.New("wake failed"),
		results: []TransportResult{{Outcome: OutcomeAsleep}, {Outcome: OutcomeOK}},
	}
	e := newExec(tr)
	res, err := e.Execute(context.Background(), Request{VIN: "V", Command: "door_lock", Scopes: grantAll()})
	if err != nil {
		t.Fatalf("a wake error must not abort the retry loop: %v", err)
	}
	if !res.Applied {
		t.Fatalf("expected applied")
	}
}

func TestExecute_SessionRefreshOnCounterError(t *testing.T) {
	tr := &fakeTransport{enabled: true, results: []TransportResult{
		{Outcome: OutcomeCounterError},
		{Outcome: OutcomeOK},
	}}
	e := newExec(tr)
	res, err := e.Execute(context.Background(), Request{VIN: "V", Command: "door_lock", Scopes: grantAll()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Applied {
		t.Fatalf("expected applied after one counter retry")
	}
	if len(tr.calls) != 2 {
		t.Fatalf("expected 2 attempts, got %d", len(tr.calls))
	}
}

func TestExecute_CounterErrorExhausted(t *testing.T) {
	tr := &fakeTransport{enabled: true, results: []TransportResult{{Outcome: OutcomeCounterError}}}
	e := newExec(tr)
	_, err := e.Execute(context.Background(), Request{VIN: "V", Command: "door_lock", Scopes: grantAll()})
	wantCode(t, err, wserrors.ErrCodeCommandFailed)
	if len(tr.calls) != fastConfig().CounterRetryMax+1 {
		t.Fatalf("attempts = %d want %d", len(tr.calls), fastConfig().CounterRetryMax+1)
	}
}

func TestExecute_OutcomeMapping(t *testing.T) {
	tests := []struct {
		name    string
		outcome Outcome
		code    wserrors.ErrorCode
	}{
		{"not paired", OutcomeNotPaired, wserrors.ErrCodeKeyNotPaired},
		{"permission denied", OutcomePermissionDenied, wserrors.ErrCodePermissionDenied},
		{"invalid request", OutcomeInvalidRequest, wserrors.ErrCodeInvalidRequest},
		{"failed", OutcomeFailed, wserrors.ErrCodeCommandFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := &fakeTransport{enabled: true, results: []TransportResult{{Outcome: tt.outcome}}}
			e := newExec(tr)
			_, err := e.Execute(context.Background(), Request{VIN: "V", Command: "door_lock", Scopes: grantAll()})
			wantCode(t, err, tt.code)
		})
	}
}

// TestExecute_ThreadsTransportReasonAsDetail proves the opaque Tesla/proxy
// reason on a terminal outcome is threaded onto CommandError.Detail (MYR-245
// observability) — feeding the dispatch outcome log without changing the wire
// code. Covers each terminal branch that carries a res.Reason.
func TestExecute_ThreadsTransportReasonAsDetail(t *testing.T) {
	tests := []struct {
		name    string
		outcome Outcome
		reason  string
		code    wserrors.ErrorCode
	}{
		{"invalid request", OutcomeInvalidRequest, "invalid_command", wserrors.ErrCodeInvalidRequest},
		{"not paired", OutcomeNotPaired, "keys_not_configured", wserrors.ErrCodeKeyNotPaired},
		{"permission denied", OutcomePermissionDenied, "missing scope", wserrors.ErrCodePermissionDenied},
		{"failed", OutcomeFailed, "vehicle error 500", wserrors.ErrCodeCommandFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := &fakeTransport{enabled: true, results: []TransportResult{{Outcome: tt.outcome, Reason: tt.reason}}}
			e := newExec(tr)
			_, err := e.Execute(context.Background(), Request{VIN: "V", Command: "door_lock", Scopes: grantAll()})
			var cErr *CommandError
			if !asCommandError(err, &cErr) {
				t.Fatalf("error is not *CommandError: %v", err)
			}
			if cErr.Code != tt.code {
				t.Errorf("code = %q want %q", cErr.Code, tt.code)
			}
			if cErr.Detail != tt.reason {
				t.Errorf("detail = %q want %q", cErr.Detail, tt.reason)
			}
		})
	}
}

func TestExecute_InvalidParamsBeforeTransport(t *testing.T) {
	tr := &fakeTransport{enabled: true, results: []TransportResult{{Outcome: OutcomeOK}}}
	e := newExec(tr)
	_, err := e.Execute(context.Background(), Request{
		VIN: "V", Command: "set_charge_limit",
		Params: map[string]any{"percent": 10.0}, Scopes: grantAll(),
	})
	wantCode(t, err, wserrors.ErrCodeInvalidRequest)
	if len(tr.calls) != 0 {
		t.Fatalf("invalid params must not reach the transport")
	}
}

func TestExecute_TransportErrorIsCommandFailed(t *testing.T) {
	tr := &fakeTransport{enabled: true, cmdErr: errors.New("boom")}
	e := newExec(tr)
	_, err := e.Execute(context.Background(), Request{VIN: "V", Command: "door_lock", Scopes: grantAll()})
	wantCode(t, err, wserrors.ErrCodeCommandFailed)
}
