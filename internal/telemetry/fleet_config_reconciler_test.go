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
	rows      []FleetConfigCandidate
	err       error
	gotCutoff time.Time
	gotLimit  int
}

func (f *fakeCandidateLister) ListFleetConfigCandidates(
	_ context.Context, cutoff time.Time, limit int,
) ([]FleetConfigCandidate, error) {
	f.gotCutoff = cutoff
	f.gotLimit = limit
	return f.rows, f.err
}

type fakeConfigReader struct {
	synced bool
	err    error
	calls  int
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
}

func (f *fakeConfigWriter) PushTelemetryConfig(
	_ context.Context, _ string, req FleetConfigRequest,
) (*FleetConfigResponse, error) {
	f.calls++
	f.gotReq = req
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
	return NewFleetConfigReconciler(
		FleetConfigReconcilerDeps{
			Candidates: lister,
			Reader:     reader,
			Writer:     writer,
			Tokens:     tokens,
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
				// Synced yet quiet is a DIFFERENT fault and must be surfaced.
				if out.SyncedNotStream != 1 {
					t.Errorf("SyncedNotStream = %d, want 1", out.SyncedNotStream)
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
