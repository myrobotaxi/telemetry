package commands

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// scriptedProbe returns awake=false for the first `asleepFor` calls, then true.
// A negative asleepFor never wakes.
type scriptedProbe struct {
	asleepFor int
	calls     int
	err       error
}

func (p *scriptedProbe) probe(context.Context) (bool, error) {
	p.calls++
	if p.err != nil {
		return false, p.err
	}
	if p.asleepFor < 0 || p.calls <= p.asleepFor {
		return false, nil
	}
	return true, nil
}

func newWakeExecutor(tr *fakeTransport) *Executor {
	return NewExecutor(tr, nil, WithConfig(fastConfig()))
}

// An already-awake vehicle must cost exactly one probe and ZERO wake calls —
// the property the MYR-315 refresh endpoint depends on so tapping "refresh" on
// a healthy car never disturbs it.
func TestEnsureAwake_AlreadyAwakeNeverWakes(t *testing.T) {
	tr := &fakeTransport{enabled: true}
	p := &scriptedProbe{asleepFor: 0}

	if err := newWakeExecutor(tr).EnsureAwake(context.Background(), "vin", "tok", p.probe); err != nil {
		t.Fatalf("EnsureAwake: %v", err)
	}
	if p.calls != 1 {
		t.Fatalf("probe calls = %d want 1", p.calls)
	}
	if tr.wakes != 0 {
		t.Fatalf("wakes = %d want 0", tr.wakes)
	}
}

// A car that wakes partway through must succeed within budget, spending one
// wake per failed probe.
func TestEnsureAwake_WakesThenSucceeds(t *testing.T) {
	tr := &fakeTransport{enabled: true}
	p := &scriptedProbe{asleepFor: 2}

	if err := newWakeExecutor(tr).EnsureAwake(context.Background(), "vin", "tok", p.probe); err != nil {
		t.Fatalf("EnsureAwake: %v", err)
	}
	if p.calls != 3 {
		t.Fatalf("probe calls = %d want 3", p.calls)
	}
	if tr.wakes != 2 {
		t.Fatalf("wakes = %d want 2", tr.wakes)
	}
}

// Budget exhaustion is the contract the refresh endpoint's 503 rests on: at
// most WakeMaxAttempts wakes, WakeMaxAttempts+1 probes, then the same typed
// vehicle_asleep error the command path produces.
func TestEnsureAwake_BudgetExhaustedYieldsVehicleAsleep(t *testing.T) {
	tr := &fakeTransport{enabled: true}
	p := &scriptedProbe{asleepFor: -1}

	err := newWakeExecutor(tr).EnsureAwake(context.Background(), "vin", "tok", p.probe)
	if err == nil {
		t.Fatal("expected an error once the wake budget is spent")
	}
	wantCode(t, err, wserrors.ErrCodeVehicleAsleep)

	var cErr *CommandError
	if !asCommandError(err, &cErr) {
		t.Fatalf("error is not *CommandError: %v", err)
	}
	if cErr.Status != 503 {
		t.Fatalf("status = %d want 503", cErr.Status)
	}
	if !cErr.Retryable {
		t.Fatal("vehicle_asleep must be Retryable so the SDK backs off")
	}
	if tr.wakes != fastConfig().WakeMaxAttempts {
		t.Fatalf("wakes = %d want %d", tr.wakes, fastConfig().WakeMaxAttempts)
	}
	if p.calls != fastConfig().WakeMaxAttempts+1 {
		t.Fatalf("probe calls = %d want %d", p.calls, fastConfig().WakeMaxAttempts+1)
	}
}

// A wake call that itself errors is best-effort: the loop keeps its budget and
// still lets the car come up, mirroring Execute's asleep branch.
func TestEnsureAwake_SurvivesWakeError(t *testing.T) {
	tr := &fakeTransport{enabled: true, wakeErr: errors.New("wake refused")}
	p := &scriptedProbe{asleepFor: 1}

	if err := newWakeExecutor(tr).EnsureAwake(context.Background(), "vin", "tok", p.probe); err != nil {
		t.Fatalf("EnsureAwake: %v", err)
	}
	if tr.wakes != 1 {
		t.Fatalf("wakes = %d want 1", tr.wakes)
	}
}

// A failing probe is terminal, not budget-burning: a wake cannot fix a Fleet
// API that is refusing to answer.
func TestEnsureAwake_ProbeErrorIsTerminal(t *testing.T) {
	tr := &fakeTransport{enabled: true}
	p := &scriptedProbe{err: errors.New("fleet 500")}

	err := newWakeExecutor(tr).EnsureAwake(context.Background(), "vin", "tok", p.probe)
	wantCode(t, err, wserrors.ErrCodeCommandFailed)
	if tr.wakes != 0 {
		t.Fatalf("wakes = %d want 0", tr.wakes)
	}
}

// With no wake transport configured, an awake car still succeeds (the check is
// lazy) but a sleeping one fails fast with the permanent sentinel rather than
// burning the budget against a transport that cannot deliver.
func TestEnsureAwake_UnconfiguredTransport(t *testing.T) {
	t.Run("awake car still succeeds", func(t *testing.T) {
		tr := &fakeTransport{enabled: false}
		p := &scriptedProbe{asleepFor: 0}
		if err := newWakeExecutor(tr).EnsureAwake(context.Background(), "vin", "tok", p.probe); err != nil {
			t.Fatalf("EnsureAwake: %v", err)
		}
	})

	t.Run("sleeping car fails fast", func(t *testing.T) {
		tr := &fakeTransport{enabled: false}
		p := &scriptedProbe{asleepFor: -1}
		err := newWakeExecutor(tr).EnsureAwake(context.Background(), "vin", "tok", p.probe)
		if !errors.Is(err, ErrTransportNotConfigured) {
			t.Fatalf("error = %v, want ErrTransportNotConfigured", err)
		}
		if tr.wakes != 0 {
			t.Fatalf("wakes = %d want 0", tr.wakes)
		}
	})
}

// A canceled context stops the loop instead of spinning out the full budget.
func TestEnsureAwake_ContextCancellation(t *testing.T) {
	tr := &fakeTransport{enabled: true}
	ctx, cancel := context.WithCancel(context.Background())

	slow := NewExecutor(tr, nil, WithConfig(Config{
		WakeMaxAttempts: 3,
		WakeBackoff:     time.Hour, // never elapses; cancellation must break out
		CounterRetryMax: 1,
	}))

	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	err := slow.EnsureAwake(ctx, "vin", "tok", func(context.Context) (bool, error) { return false, nil })
	wantCode(t, err, wserrors.ErrCodeInternalError)
}
