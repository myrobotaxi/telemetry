package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"
)

const reconTestVIN = "7SAYGDED5TA736164"

// --- fakes -----------------------------------------------------------------

type fakeCandidateLister struct {
	rows        []FleetConfigCandidate
	err         error
	gotCutoff   time.Time
	gotHotSince time.Time
	gotLimit    int
}

func (f *fakeCandidateLister) ListFleetConfigCandidates(
	_ context.Context, cutoff, _, hotSince time.Time, limit int,
) ([]FleetConfigCandidate, error) {
	f.gotCutoff = cutoff
	f.gotHotSince = hotSince
	f.gotLimit = limit
	return f.rows, f.err
}

// recordedAttempt is one call to RecordFleetConfigAttempt.
type recordedAttempt struct {
	vehicleID string
	next      time.Time
	outcome   string
}

type fakeAttemptRecorder struct {
	recorded []recordedAttempt
	cleared  []string
	recErr   error
	// forced records the MYR-489 escalation writes, kept separate from
	// `recorded` so a test can assert that a forced re-push stamped the epoch
	// budget rather than taking the ordinary path.
	forced    []recordedAttempt
	forcedErr error
}

func (f *fakeAttemptRecorder) RecordForcedFleetConfigRepush(
	_ context.Context, vehicleID string, _, nextAttemptAt time.Time, outcome string,
) error {
	f.forced = append(f.forced, recordedAttempt{vehicleID: vehicleID, next: nextAttemptAt, outcome: outcome})
	return f.forcedErr
}

func (f *fakeAttemptRecorder) RecordFleetConfigAttempt(
	_ context.Context, vehicleID string, _, nextAttemptAt time.Time, outcome string,
) error {
	f.recorded = append(f.recorded, recordedAttempt{vehicleID: vehicleID, next: nextAttemptAt, outcome: outcome})
	return f.recErr
}

func (f *fakeAttemptRecorder) ClearFleetConfigAttempts(_ context.Context, vehicleID string) error {
	f.cleared = append(f.cleared, vehicleID)
	return nil
}

type fakeConfigReader struct {
	synced bool
	err    error
	calls  int
	// MYR-489 awake probe.
	vehicleState string
	vehicleErr   error
	vehicleCalls int
}

func (f *fakeConfigReader) GetVehicle(_ context.Context, _, vin string) (*FleetVehicleState, error) {
	f.vehicleCalls++
	if f.vehicleErr != nil {
		return nil, f.vehicleErr
	}
	return &FleetVehicleState{VIN: vin, State: f.vehicleState}, nil
}

func (f *fakeConfigReader) GetTelemetryConfig(_ context.Context, _, _ string) (*FleetConfigStatusResponse, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &FleetConfigStatusResponse{Response: FleetConfigStatus{Synced: f.synced}}, nil
}

type fakeConfigWriter struct {
	result *FleetConfigResponse
	err    error
	calls  int
	gotReq FleetConfigRequest
	// MYR-489 delete-then-create.
	deleteCalls int
	deleteErr   error
	// order records the sequence of write verbs so a test can prove the DELETE
	// preceded the POST rather than merely that both happened.
	order []string
}

func (f *fakeConfigWriter) DeleteTelemetryConfig(_ context.Context, _, _ string) error {
	f.deleteCalls++
	f.order = append(f.order, "delete")
	return f.deleteErr
}

func (f *fakeConfigWriter) PushTelemetryConfig(
	_ context.Context, _ string, req FleetConfigRequest,
) (*FleetConfigResponse, error) {
	f.calls++
	f.gotReq = req
	f.order = append(f.order, "push")
	return f.result, f.err
}

type fakeReconTokenResolver struct {
	tok TeslaToken
	err error
}

func (f *fakeReconTokenResolver) Resolve(_ context.Context, _ string) (TeslaToken, error) {
	return f.tok, f.err
}

func newTestReconciler(
	lister *fakeCandidateLister,
	reader *fakeConfigReader,
	writer *fakeConfigWriter,
	tokens *fakeReconTokenResolver,
) *FleetConfigReconciler {
	return newTestReconcilerWithAttempts(lister, reader, writer, tokens, &fakeAttemptRecorder{})
}

func newTestReconcilerWithAttempts(
	lister *fakeCandidateLister,
	reader *fakeConfigReader,
	writer *fakeConfigWriter,
	tokens *fakeReconTokenResolver,
	attempts *fakeAttemptRecorder,
) *FleetConfigReconciler {
	return NewFleetConfigReconciler(
		FleetConfigReconcilerDeps{
			Candidates: lister,
			Attempts:   attempts,
			Reader:     reader,
			Writer:     writer,
			Tokens:     tokens,
			Pairing:    &fakePairingResetter{},
		},
		FleetConfigReconcileConfig{},
		EndpointConfig{Hostname: "telemetry.myrobotaxi.app", Port: 443, CA: "-----BEGIN CERTIFICATE-----"},
		slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	)
}

func oneCandidate() []FleetConfigCandidate {
	return []FleetConfigCandidate{{
		VehicleID:   "veh-1",
		VIN:         reconTestVIN,
		UserID:      "user-1",
		LastUpdated: time.Date(2026, 8, 6, 2, 58, 37, 0, time.UTC),
	}}
}

func okToken() *fakeReconTokenResolver {
	return &fakeReconTokenResolver{tok: TeslaToken{AccessToken: "at"}}
}

// --- tests -----------------------------------------------------------------

func TestReconcileOutcomes(t *testing.T) {
	tests := []struct {
		name       string
		reader     *fakeConfigReader
		writer     *fakeConfigWriter
		tokens     *fakeReconTokenResolver
		wantPushes int
		check      func(t *testing.T, out ReconcileOutcome)
	}{
		{
			name:   "unconfigured car is repaired",
			reader: &fakeConfigReader{synced: false},
			writer: &fakeConfigWriter{result: &FleetConfigResponse{
				Response: FleetConfigResult{UpdatedVehicles: 1},
			}},
			tokens:     okToken(),
			wantPushes: 1,
			check: func(t *testing.T, out ReconcileOutcome) {
				if out.Repaired != 1 {
					t.Errorf("Repaired = %d, want 1", out.Repaired)
				}
			},
		},
		{
			// The MYR-448 case: Tesla says 200 but applied nothing.
			name:   "skipped for missing_key counts as awaiting pairing, not repaired",
			reader: &fakeConfigReader{synced: false},
			writer: &fakeConfigWriter{result: &FleetConfigResponse{
				Response: FleetConfigResult{
					UpdatedVehicles: 0,
					SkippedVehicles: map[string]string{reconTestVIN: SkipReasonMissingKey},
				},
			}},
			tokens:     okToken(),
			wantPushes: 1,
			check: func(t *testing.T, out ReconcileOutcome) {
				if out.AwaitingKey != 1 {
					t.Errorf("AwaitingKey = %d, want 1", out.AwaitingKey)
				}
				if out.Repaired != 0 {
					t.Errorf("Repaired = %d, want 0 — a skip must never read as success", out.Repaired)
				}
			},
		},
		{
			name:   "skipped for an unknown reason is counted separately",
			reader: &fakeConfigReader{synced: false},
			writer: &fakeConfigWriter{result: &FleetConfigResponse{
				Response: FleetConfigResult{
					SkippedVehicles: map[string]string{reconTestVIN: "unsupported_firmware"},
				},
			}},
			tokens:     okToken(),
			wantPushes: 1,
			check: func(t *testing.T, out ReconcileOutcome) {
				if out.SkippedOther != 1 {
					t.Errorf("SkippedOther = %d, want 1", out.SkippedOther)
				}
				if out.AwaitingKey != 0 {
					t.Errorf("AwaitingKey = %d, want 0", out.AwaitingKey)
				}
			},
		},
		{
			name:       "already-synced car is never pushed to",
			reader:     &fakeConfigReader{synced: true},
			writer:     &fakeConfigWriter{},
			tokens:     okToken(),
			wantPushes: 0,
			check: func(t *testing.T, out ReconcileOutcome) {
				if out.AlreadySynced != 1 {
					t.Errorf("AlreadySynced = %d, want 1", out.AlreadySynced)
				}
			},
		},
		{
			name:       "a failed config read never falls through to a blind push",
			reader:     &fakeConfigReader{err: errors.New("fleet api 500")},
			writer:     &fakeConfigWriter{},
			tokens:     okToken(),
			wantPushes: 0,
			check: func(t *testing.T, out ReconcileOutcome) {
				if out.ReadFailures != 1 {
					t.Errorf("ReadFailures = %d, want 1", out.ReadFailures)
				}
			},
		},
		{
			name:       "an unresolvable token skips the car without touching Tesla",
			reader:     &fakeConfigReader{},
			writer:     &fakeConfigWriter{},
			tokens:     &fakeReconTokenResolver{err: ErrTeslaTokenExpired},
			wantPushes: 0,
			check: func(t *testing.T, out ReconcileOutcome) {
				if out.TokenFailures != 1 {
					t.Errorf("TokenFailures = %d, want 1", out.TokenFailures)
				}
			},
		},
		{
			name:       "a push transport error is counted and retried next pass",
			reader:     &fakeConfigReader{synced: false},
			writer:     &fakeConfigWriter{err: errors.New("connection refused")},
			tokens:     okToken(),
			wantPushes: 1,
			check: func(t *testing.T, out ReconcileOutcome) {
				if out.PushFailures != 1 {
					t.Errorf("PushFailures = %d, want 1", out.PushFailures)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			lister := &fakeCandidateLister{rows: oneCandidate()}
			r := newTestReconciler(lister, tc.reader, tc.writer, tc.tokens)

			out, err := r.Reconcile(context.Background())
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if out.Examined != 1 {
				t.Errorf("Examined = %d, want 1", out.Examined)
			}
			if tc.writer.calls != tc.wantPushes {
				t.Errorf("push calls = %d, want %d", tc.writer.calls, tc.wantPushes)
			}
			tc.check(t, out)
		})
	}
}

// A LIST failure must change nothing: a DB blip must never be read as "no car
// needs healing", and it must not be silently swallowed either.
func TestReconcileListFailureIsReturned(t *testing.T) {
	lister := &fakeCandidateLister{err: errors.New("db down")}
	writer := &fakeConfigWriter{}
	r := newTestReconciler(lister, &fakeConfigReader{}, writer, okToken())

	_, err := r.Reconcile(context.Background())
	if err == nil {
		t.Fatal("Reconcile() error = nil, want the list error")
	}
	if writer.calls != 0 {
		t.Errorf("push calls = %d, want 0 — a list failure must change nothing", writer.calls)
	}
}

func TestReconcileNoCandidatesIsCheap(t *testing.T) {
	lister := &fakeCandidateLister{rows: nil}
	reader := &fakeConfigReader{}
	writer := &fakeConfigWriter{}
	r := newTestReconciler(lister, reader, writer, okToken())

	out, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if out.Examined != 0 || reader.calls != 0 || writer.calls != 0 {
		t.Errorf("examined=%d reads=%d pushes=%d, want all zero", out.Examined, reader.calls, writer.calls)
	}
}

// The pushed body must match what the link hook, the REST handler and
// `ops fleet-config push` send, so a reconciled car is configured identically
// to a hand-pushed one.
func TestReconcilePushRequestShape(t *testing.T) {
	writer := &fakeConfigWriter{result: &FleetConfigResponse{
		Response: FleetConfigResult{UpdatedVehicles: 1},
	}}
	r := newTestReconciler(
		&fakeCandidateLister{rows: oneCandidate()},
		&fakeConfigReader{synced: false},
		writer,
		okToken(),
	)

	if _, err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	req := writer.gotReq
	if len(req.VINs) != 1 || req.VINs[0] != reconTestVIN {
		t.Errorf("VINs = %v, want exactly [%s]", req.VINs, reconTestVIN)
	}
	if req.Config.Hostname != "telemetry.myrobotaxi.app" || req.Config.Port != 443 {
		t.Errorf("endpoint = %s:%d, want telemetry.myrobotaxi.app:443", req.Config.Hostname, req.Config.Port)
	}
	if req.Config.CA == nil || *req.Config.CA == "" {
		t.Error("CA should be forwarded when configured")
	}
	if len(req.Config.Fields) == 0 {
		t.Error("Fields must carry DefaultFieldConfig, got empty")
	}
	if req.Config.Exp == nil {
		t.Fatal("Exp must be set — Tesla rejects a config without one")
	}
	// Tesla requires exp between ~31 and ~360 days out.
	days := time.Until(time.Unix(*req.Config.Exp, 0)).Hours() / 24
	if days < 31 || days > 360 {
		t.Errorf("exp is %.0f days out, want within Tesla's 31..360 window", days)
	}
}

// The candidate query must be asked for a cutoff in the PAST — asking for a
// future cutoff would sweep the whole fleet on every pass.
func TestReconcileUsesStalenessCutoffAndLimit(t *testing.T) {
	lister := &fakeCandidateLister{}
	r := newTestReconciler(lister, &fakeConfigReader{}, &fakeConfigWriter{}, okToken())

	before := time.Now()
	if _, err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if !lister.gotCutoff.Before(before) {
		t.Errorf("cutoff = %v, want strictly before now (%v)", lister.gotCutoff, before)
	}
	// `before` is sampled just ahead of the call, so the measured gap is the
	// staleness window minus the few microseconds in between; allow a second
	// of slack rather than asserting an exact equality against wall time.
	if gap := before.Sub(lister.gotCutoff); gap < defaultFleetConfigStaleness-time.Second {
		t.Errorf("cutoff is only %v old, want ~the %v staleness window",
			gap, defaultFleetConfigStaleness)
	}
	if lister.gotLimit != defaultFleetConfigMaxPerPass {
		t.Errorf("limit = %d, want the default cap %d", lister.gotLimit, defaultFleetConfigMaxPerPass)
	}
}

// One bad car must never abort the pass — the whole point of a fleet-wide
// reconciler is that a single owner's expired token cannot strand everyone.
func TestReconcileContinuesPastAFailingVehicle(t *testing.T) {
	lister := &fakeCandidateLister{rows: []FleetConfigCandidate{
		{VehicleID: "veh-1", VIN: reconTestVIN, UserID: "user-1"},
		{VehicleID: "veh-2", VIN: "5YJ3E1EA7JF000001", UserID: "user-2"},
		{VehicleID: "veh-3", VIN: "5YJ3E1EA7JF000002", UserID: "user-3"},
	}}
	reader := &fakeConfigReader{err: errors.New("fleet api down")}
	r := newTestReconciler(lister, reader, &fakeConfigWriter{}, okToken())

	out, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if out.Examined != 3 {
		t.Errorf("Examined = %d, want 3 — the pass must not stop at the first failure", out.Examined)
	}
	if out.ReadFailures != 3 {
		t.Errorf("ReadFailures = %d, want 3", out.ReadFailures)
	}
}

func TestFleetConfigReconcileConfigDefaults(t *testing.T) {
	tests := []struct {
		name string
		in   FleetConfigReconcileConfig
	}{
		{"zero value", FleetConfigReconcileConfig{}},
		{"negative values degrade to defaults", FleetConfigReconcileConfig{
			Interval: -time.Second, Staleness: -time.Second, MaxPerPass: -1, CallTimeout: -time.Second,
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.withDefaults()
			if got.Interval != defaultFleetConfigReconcileInterval {
				t.Errorf("Interval = %v, want %v", got.Interval, defaultFleetConfigReconcileInterval)
			}
			if got.Staleness != defaultFleetConfigStaleness {
				t.Errorf("Staleness = %v, want %v", got.Staleness, defaultFleetConfigStaleness)
			}
			if got.MaxPerPass != defaultFleetConfigMaxPerPass {
				t.Errorf("MaxPerPass = %d, want %d", got.MaxPerPass, defaultFleetConfigMaxPerPass)
			}
			if got.CallTimeout != defaultFleetConfigCallTimeout {
				t.Errorf("CallTimeout = %v, want %v", got.CallTimeout, defaultFleetConfigCallTimeout)
			}
		})
	}
}

// A cancelled context must stop the pass promptly rather than working through
// every remaining candidate.
func TestReconcileStopsOnContextCancel(t *testing.T) {
	lister := &fakeCandidateLister{rows: []FleetConfigCandidate{
		{VehicleID: "veh-1", VIN: reconTestVIN, UserID: "user-1"},
		{VehicleID: "veh-2", VIN: "5YJ3E1EA7JF000001", UserID: "user-2"},
	}}
	reader := &fakeConfigReader{synced: true}
	r := newTestReconciler(lister, reader, &fakeConfigWriter{}, okToken())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out, err := r.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if out.Examined != 0 {
		t.Errorf("Examined = %d, want 0 on an already-cancelled context", out.Examined)
	}
}

// --- MYR-448 adversarial-review regressions ---------------------------------

// C1: ordering by "Vehicle"."lastUpdated" would let unfixable cars occupy the
// per-pass limit forever, because nothing the reconciler does moves that
// column. Every non-repair outcome must therefore push the vehicle's own
// schedule forward, or the next pass returns the identical rows.
func TestEveryNonRepairOutcomeReschedules(t *testing.T) {
	tests := []struct {
		name        string
		reader      *fakeConfigReader
		writer      *fakeConfigWriter
		tokens      *fakeReconTokenResolver
		wantOutcome string
	}{
		{
			name:        "awaiting virtual key",
			reader:      &fakeConfigReader{synced: false},
			writer:      skipWriter(SkipReasonMissingKey),
			tokens:      okToken(),
			wantOutcome: outcomeAwaitingKey,
		},
		{
			name:        "unrecognised skip",
			reader:      &fakeConfigReader{synced: false},
			writer:      skipWriter("unsupported_hardware"),
			tokens:      okToken(),
			wantOutcome: outcomeSkippedOther,
		},
		{
			name:        "config read failed",
			reader:      &fakeConfigReader{err: errors.New("boom")},
			writer:      &fakeConfigWriter{},
			tokens:      okToken(),
			wantOutcome: outcomeReadFailed,
		},
		{
			name:        "push failed",
			reader:      &fakeConfigReader{synced: false},
			writer:      &fakeConfigWriter{err: errors.New("boom")},
			tokens:      okToken(),
			wantOutcome: outcomePushFailed,
		},
		{
			name:        "token unusable",
			reader:      &fakeConfigReader{},
			writer:      &fakeConfigWriter{},
			tokens:      &fakeReconTokenResolver{err: ErrTeslaTokenExpired},
			wantOutcome: outcomeTokenFailed,
		},
		{
			name:        "synced but quiet",
			reader:      &fakeConfigReader{synced: true},
			writer:      &fakeConfigWriter{},
			tokens:      okToken(),
			wantOutcome: outcomeSyncedQuiet,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			attempts := &fakeAttemptRecorder{}
			r := newTestReconcilerWithAttempts(
				&fakeCandidateLister{rows: oneCandidate()}, tc.reader, tc.writer, tc.tokens, attempts)

			if _, err := r.Reconcile(context.Background()); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}

			if len(attempts.recorded) != 1 {
				t.Fatalf("recorded %d attempts, want 1 — an un-rescheduled vehicle "+
					"stays permanently due and starves the rest of the fleet", len(attempts.recorded))
			}
			got := attempts.recorded[0]
			if got.vehicleID != "veh-1" {
				t.Errorf("rescheduled %q, want veh-1", got.vehicleID)
			}
			if got.outcome != tc.wantOutcome {
				t.Errorf("outcome = %q, want %q", got.outcome, tc.wantOutcome)
			}
			if !got.next.After(time.Now()) {
				t.Errorf("next attempt %v is not in the future — that is a hot loop", got.next)
			}
			if len(attempts.cleared) != 0 {
				t.Errorf("cleared %v on a non-repair outcome", attempts.cleared)
			}
		})
	}
}

// A repair drops the schedule row: the car should stream now, and if it ever
// goes quiet again it deserves a fresh count rather than an inherited backoff.
func TestRepairClearsTheSchedule(t *testing.T) {
	attempts := &fakeAttemptRecorder{}
	writer := &fakeConfigWriter{result: &FleetConfigResponse{
		Response: FleetConfigResult{UpdatedVehicles: 1},
	}}
	r := newTestReconcilerWithAttempts(
		&fakeCandidateLister{rows: oneCandidate()},
		&fakeConfigReader{synced: false}, writer, okToken(), attempts)

	if _, err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(attempts.cleared) != 1 || attempts.cleared[0] != "veh-1" {
		t.Errorf("cleared = %v, want [veh-1]", attempts.cleared)
	}
	if len(attempts.recorded) != 0 {
		t.Errorf("recorded %v on a successful repair", attempts.recorded)
	}
}

// H1: a car that can never be fixed must cost progressively less, or an
// unpaired owner is ~96 signed POSTs a day against Tesla forever.
func TestBackoffGrowsAndIsCapped(t *testing.T) {
	cfg := FleetConfigReconcileConfig{}.withDefaults()

	if got := cfg.backoffFor(0); got != cfg.Interval {
		t.Errorf("backoffFor(0) = %v, want the base interval %v", got, cfg.Interval)
	}
	if got := cfg.backoffFor(1); got != 2*cfg.Interval {
		t.Errorf("backoffFor(1) = %v, want %v", got, 2*cfg.Interval)
	}

	prev := cfg.backoffFor(0)
	for n := 1; n <= 8; n++ {
		got := cfg.backoffFor(n)
		if got < prev {
			t.Errorf("backoffFor(%d) = %v shrank from %v", n, got, prev)
		}
		if got > cfg.MaxBackoff {
			t.Errorf("backoffFor(%d) = %v exceeds the cap %v", n, got, cfg.MaxBackoff)
		}
		prev = got
	}

	// A count read back from the DB must never overflow the shift into a zero
	// or negative delay, which would mean "retry immediately, forever".
	for _, n := range []int{31, 32, 63, 64, 1 << 20} {
		got := cfg.backoffFor(n)
		if got != cfg.MaxBackoff {
			t.Errorf("backoffFor(%d) = %v, want the cap %v", n, got, cfg.MaxBackoff)
		}
	}
}

// M4: a pass cut short must not log identically to a complete one.
func TestTruncatedPassIsFlagged(t *testing.T) {
	lister := &fakeCandidateLister{rows: []FleetConfigCandidate{
		{VehicleID: "veh-1", VIN: reconTestVIN, UserID: "user-1"},
	}}
	r := newTestReconciler(lister, &fakeConfigReader{synced: true}, &fakeConfigWriter{}, okToken())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out, err := r.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !out.Truncated {
		t.Error("Truncated = false on a cancelled pass; an operator cannot tell it did nothing")
	}

	full, err := r.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if full.Truncated {
		t.Error("Truncated = true on a pass that examined every candidate")
	}
}

func skipWriter(reason string) *fakeConfigWriter {
	return &fakeConfigWriter{result: &FleetConfigResponse{
		Response: FleetConfigResult{
			UpdatedVehicles: 0,
			SkippedVehicles: map[string]string{reconTestVIN: reason},
		},
	}}
}
