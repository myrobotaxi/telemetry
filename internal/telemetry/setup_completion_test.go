package telemetry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// setupNow is the reference instant. Fixed so every `since` assertion is exact.
var setupNow = time.Date(2026, 8, 9, 4, 0, 0, 0, time.UTC)

// --- fakes -----------------------------------------------------------------

// fakeSetupProbe stands in for the DIRECT Fleet API client. Per-probe scripting
// (`answers`) is what lets a test prove the poll loop retries a flaky read
// rather than merely that it called Tesla once.
type fakeSetupProbe struct {
	mu sync.Mutex

	wakeErr   error
	wakeState string
	wakeCalls int

	// answers is consumed one entry per probe; the last entry repeats.
	answers     []probeAnswer
	statusCalls int
	gotVINs     []string
}

type probeAnswer struct {
	paired bool
	err    error
}

func (f *fakeSetupProbe) WakeVehicle(_ context.Context, _, vin string) (*FleetVehicleState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.wakeCalls++
	if f.wakeErr != nil {
		return nil, f.wakeErr
	}
	return &FleetVehicleState{VIN: vin, State: f.wakeState}, nil
}

func (f *fakeSetupProbe) GetFleetStatus(_ context.Context, _ string, vins []string) (*FleetStatusResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls++
	f.gotVINs = append(f.gotVINs, vins...)

	a := probeAnswer{}
	if len(f.answers) > 0 {
		idx := f.statusCalls - 1
		if idx >= len(f.answers) {
			idx = len(f.answers) - 1
		}
		a = f.answers[idx]
	}
	if a.err != nil {
		return nil, a.err
	}
	resp := &FleetStatusResponse{}
	if a.paired {
		resp.Response.KeyPairedVINs = []string{vins[0]}
	} else {
		resp.Response.UnpairedVINs = []string{vins[0]}
	}
	return resp, nil
}

func (f *fakeSetupProbe) counts() (wake, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.wakeCalls, f.statusCalls
}

// fakeSnapshotReader serves GetByID. `rows` lets a test hand back a DIFFERENT
// row on the re-read than on the first read, which is how the
// reconciler-raced-us case is exercised.
type fakeSnapshotReader struct {
	mu    sync.Mutex
	rows  []VehicleSnapshotRow
	err   error
	calls int
}

func (f *fakeSnapshotReader) GetByID(_ context.Context, _ string) (VehicleSnapshotRow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return VehicleSnapshotRow{}, f.err
	}
	idx := f.calls - 1
	if idx >= len(f.rows) {
		idx = len(f.rows) - 1
	}
	return f.rows[idx], nil
}

// fakeRepusher records forced re-pushes and the candidate each was handed.
type fakeRepusher struct {
	mu      sync.Mutex
	applied bool
	calls   int
	got     []FleetConfigCandidate
}

func (f *fakeRepusher) ForceConfigRepushNow(_ context.Context, c FleetConfigCandidate, _ string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.got = append(f.got, c)
	return f.applied
}

func (f *fakeRepusher) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// --- harness ---------------------------------------------------------------

// setupRow is a linked car that has NEVER streamed: the schema-default status
// MYR-437 describes, a stale row, and a seeded awaiting-key schedule.
func setupRow(mutate func(*VehicleSnapshotRow)) VehicleSnapshotRow {
	r := VehicleSnapshotRow{
		ID:          "veh-1",
		UserID:      "user-1",
		VIN:         reconTestVIN,
		Status:      vehicleStatusOffline,
		LastUpdated: setupNow.Add(-3 * time.Hour),
		SetupSchedule: VehicleSetupSchedule{
			Present:       true,
			LastOutcome:   outcomeAwaitingKey,
			LastAttemptAt: setupNow.Add(-40 * time.Minute),
		},
	}
	if mutate != nil {
		mutate(&r)
	}
	return r
}

type setupHarness struct {
	completer *SetupCompleter
	probe     *fakeSetupProbe
	vehicles  *fakeSnapshotReader
	pairing   *fakePairingResetter
	repusher  *fakeRepusher
	tokens    *fakeReconTokenResolver
}

func newSetupHarness(rows []VehicleSnapshotRow, mutate func(*setupHarness)) *setupHarness {
	h := &setupHarness{
		probe:    &fakeSetupProbe{wakeState: "asleep", answers: []probeAnswer{{paired: true}}},
		vehicles: &fakeSnapshotReader{rows: rows},
		pairing:  &fakePairingResetter{},
		repusher: &fakeRepusher{applied: true},
		tokens:   &fakeReconTokenResolver{tok: TeslaToken{AccessToken: "tok"}},
	}
	if mutate != nil {
		mutate(h)
	}
	deps := SetupCompleterDeps{
		Vehicles: h.vehicles,
		Tokens:   h.tokens,
		Probe:    h.probe,
		Pairing:  h.pairing,
	}
	if h.repusher != nil {
		deps.Repusher = h.repusher
	}
	// A tiny probe interval keeps the multi-probe tests fast without weakening
	// them: what is under test is the loop's decisions, not wall-clock timing.
	h.completer = NewSetupCompleter(deps, SetupCompletionConfig{
		ProbeBudget:   50 * time.Millisecond,
		ProbeInterval: time.Millisecond,
		MaxProbes:     3,
	}, nil)
	h.completer.now = func() time.Time { return setupNow }
	return h
}

// --- the sequence ----------------------------------------------------------

// TestCompleteSetupStates walks every answer the endpoint can honestly give.
func TestCompleteSetupStates(t *testing.T) {
	tests := []struct {
		name  string
		row   VehicleSnapshotRow
		setup func(*setupHarness)
		// wantState is the setupState.state, or "" when an error is expected.
		wantState string
		wantErr   error
		// wantWakes / wantPushes bound what may reach a real car.
		wantWakes  int
		wantPushes int
	}{
		{
			// The whole point: key confirmed, config re-pushed on demand.
			name:       "paired car is woken, confirmed and configured",
			row:        setupRow(nil),
			wantState:  SetupStateConfiguring,
			wantWakes:  1,
			wantPushes: 1,
		},
		{
			// A car already streaming needs nothing — and must cost NOTHING,
			// because this is the repeat call after a successful completion.
			name: "streaming car short-circuits with no Tesla traffic",
			row: setupRow(func(r *VehicleSnapshotRow) {
				r.Status = "Parked"
				r.LastUpdated = setupNow.Add(-time.Minute)
			}),
			wantState: SetupStateStreaming,
		},
		{
			// The owner genuinely has not finished the Tesla-app approval.
			name: "unpaired car reports awaiting_virtual_key and pushes nothing",
			row:  setupRow(nil),
			setup: func(h *setupHarness) {
				h.probe.answers = []probeAnswer{{paired: false}}
			},
			wantState: SetupStateAwaitingVirtualKey,
			wantWakes: 1,
		},
		{
			// Not an error: a real state with copy of its own.
			name: "unusable Tesla token reports token_failed before any Tesla call",
			row:  setupRow(nil),
			setup: func(h *setupHarness) {
				h.tokens = &fakeReconTokenResolver{err: ErrTeslaTokenExpired}
			},
			wantState: SetupStateTokenFailed,
		},
		{
			// THE ADVERSARIAL CASE. The car never wakes but the key IS paired.
			// Collapsing this into awaiting_virtual_key would send the owner
			// off to re-pair a key that is already fine.
			name: "paired car that will not wake is unreachable, NOT awaiting_virtual_key",
			row:  setupRow(nil),
			setup: func(h *setupHarness) {
				h.probe.wakeErr = errors.New("upstream 500")
			},
			wantErr:   ErrSetupVehicleUnreachable,
			wantWakes: 1,
		},
		{
			// The same dead wake, but Tesla says unpaired. Now the pairing
			// answer is the actionable one and the wake is irrelevant —
			// fleet_status is a Tesla-side record, not a car query.
			name: "unpaired car that will not wake still reports awaiting_virtual_key",
			row:  setupRow(nil),
			setup: func(h *setupHarness) {
				h.probe.wakeErr = errors.New("upstream 500")
				h.probe.answers = []probeAnswer{{paired: false}}
			},
			wantState: SetupStateAwaitingVirtualKey,
			wantWakes: 1,
		},
		{
			// No clean answer at all: UNKNOWN, never a fabricated state.
			name: "probe that never answers is an error, not awaiting_virtual_key",
			row:  setupRow(nil),
			setup: func(h *setupHarness) {
				h.probe.answers = []probeAnswer{{err: errors.New("429 rate limited")}}
			},
			wantErr:   ErrSetupProbeUnavailable,
			wantWakes: 1,
		},
		{
			// A paired car whose push fails must NOT be told it is configuring.
			name: "failed config push is an error, never configuring",
			row:  setupRow(nil),
			setup: func(h *setupHarness) {
				h.repusher.applied = false
			},
			wantErr:    ErrSetupPushFailed,
			wantWakes:  1,
			wantPushes: 1,
		},
		{
			// A deployment with no signing proxy can still CHECK pairing; it
			// simply cannot push. That is a service fact, not a car fact.
			name: "no repusher configured refuses honestly",
			row:  setupRow(nil),
			setup: func(h *setupHarness) {
				h.repusher = nil
			},
			wantErr:   ErrSetupPushUnavailable,
			wantWakes: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSetupHarness([]VehicleSnapshotRow{tt.row}, tt.setup)

			state, err := h.completer.Complete(context.Background(), tt.row)

			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
				if state.State != "" {
					t.Errorf("state = %q on an error path, want no claim at all", state.State)
				}
			default:
				if err != nil {
					t.Fatalf("Complete: %v", err)
				}
				if state.State != tt.wantState {
					t.Fatalf("state = %q, want %q", state.State, tt.wantState)
				}
				if state.Since == "" {
					t.Error("since is empty; the client renders age from it")
				}
			}

			wakes, statuses := h.probe.counts()
			if wakes != tt.wantWakes {
				t.Errorf("wake calls = %d, want %d", wakes, tt.wantWakes)
			}
			if tt.wantWakes == 0 && statuses != 0 {
				t.Errorf("fleet_status calls = %d, want 0 (the short-circuit must be free)", statuses)
			}
			if h.repusher != nil {
				if got := h.repusher.count(); got != tt.wantPushes {
					t.Errorf("forced pushes = %d, want %d", got, tt.wantPushes)
				}
			}
		})
	}
}

// TestCompleteSetupProbeRetriesThenSucceeds: one flaky read inside the budget
// must cost a retry, not the owner's answer.
func TestCompleteSetupProbeRetriesThenSucceeds(t *testing.T) {
	row := setupRow(nil)
	h := newSetupHarness([]VehicleSnapshotRow{row}, func(h *setupHarness) {
		h.probe.answers = []probeAnswer{
			{err: errors.New("429 rate limited")},
			{paired: false},
			{paired: true},
		}
	})

	state, err := h.completer.Complete(context.Background(), row)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if state.State != SetupStateConfiguring {
		t.Fatalf("state = %q, want %q", state.State, SetupStateConfiguring)
	}
	if _, statuses := h.probe.counts(); statuses != 3 {
		t.Errorf("fleet_status calls = %d, want 3 (retry, then the paired answer)", statuses)
	}
}

// TestCompleteSetupProbeBudgetIsBounded: a car that stays unpaired must not
// poll Tesla forever. MaxProbes caps the count independently of the clock.
func TestCompleteSetupProbeBudgetIsBounded(t *testing.T) {
	row := setupRow(nil)
	h := newSetupHarness([]VehicleSnapshotRow{row}, func(h *setupHarness) {
		h.probe.answers = []probeAnswer{{paired: false}}
	})

	if _, err := h.completer.Complete(context.Background(), row); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	_, statuses := h.probe.counts()
	if statuses > 3 {
		t.Errorf("fleet_status calls = %d, want at most MaxProbes=3", statuses)
	}
	if statuses < 1 {
		t.Error("the probe never ran")
	}
}

// TestCompleteSetupSendsOnlyTheOneVIN guards the reader call itself: a
// fleet_status carrying more than the vehicle under test would leak other cars
// into a per-vehicle request.
func TestCompleteSetupSendsOnlyTheOneVIN(t *testing.T) {
	row := setupRow(nil)
	h := newSetupHarness([]VehicleSnapshotRow{row}, nil)

	if _, err := h.completer.Complete(context.Background(), row); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(h.probe.gotVINs) != 1 || h.probe.gotVINs[0] != reconTestVIN {
		t.Errorf("probed VINs = %v, want exactly [%s]", h.probe.gotVINs, reconTestVIN)
	}
}
