package telemetry

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The reference instant for these tests. Fixed so a backoff assertion is exact
// rather than approximate.
var forceNow = time.Date(2026, 8, 8, 21, 30, 0, 0, time.UTC)

// syncedQuietCandidate is a car in the state MYR-489 exists for: Tesla says the
// config is synced, the car has not written a row in days, and the previous
// pass already recorded synced_not_streaming. The knobs the escalation gate
// reads are left to the caller.
func syncedQuietCandidate(mutate func(*FleetConfigCandidate)) []FleetConfigCandidate {
	c := FleetConfigCandidate{
		VehicleID:     "veh-1",
		VIN:           reconTestVIN,
		UserID:        "user-1",
		LastUpdated:   forceNow.Add(-48 * time.Hour),
		AttemptCount:  6,
		LastOutcome:   outcomeSyncedQuiet,
		LastAttemptAt: forceNow.Add(-8 * time.Hour),
		// Nabil's proof: a signed command applied hours ago.
		SignedCommandAt: forceNow.Add(-2 * time.Hour),
	}
	if mutate != nil {
		mutate(&c)
	}
	return []FleetConfigCandidate{c}
}

// forceHarness wires a reconciler over a synced-reading Tesla with a frozen
// clock, and returns the fakes the assertions read.
type forceHarness struct {
	rec      *FleetConfigReconciler
	reader   *fakeConfigReader
	writer   *fakeConfigWriter
	attempts *fakeAttemptRecorder
}

func newForceHarness(rows []FleetConfigCandidate, mutate func(*forceHarness)) *forceHarness {
	h := &forceHarness{
		reader:   &fakeConfigReader{synced: true},
		writer:   &fakeConfigWriter{result: &FleetConfigResponse{Response: FleetConfigResult{UpdatedVehicles: 1}}},
		attempts: &fakeAttemptRecorder{},
	}
	if mutate != nil {
		mutate(h)
	}
	h.rec = newTestReconcilerWithAttempts(
		&fakeCandidateLister{rows: rows}, h.reader, h.writer, okToken(), h.attempts)
	h.rec.now = func() time.Time { return forceNow }
	return h
}

func TestReconcileSyncedQuietEscalation(t *testing.T) {
	tests := []struct {
		name string
		// rows is the single candidate under test.
		rows []FleetConfigCandidate
		// setup tweaks the fakes before the reconciler is built.
		setup func(*forceHarness)
		// wantForced is whether the delete-then-create should have run.
		wantForced bool
		// wantVehicleProbe is whether the awake check should have cost a
		// GetVehicle read.
		wantVehicleProbe bool
		wantOutcome      string
	}{
		{
			// The headline case. Nabil's car: awake (a signed command applied
			// two hours ago), synced at Tesla, silent for two days, and this is
			// the second consecutive synced-quiet observation.
			name:        "awake and repeatedly synced-quiet forces one re-push",
			rows:        syncedQuietCandidate(nil),
			wantForced:  true,
			wantOutcome: outcomeForcedRepush,
		},
		{
			// James Guan's car: newly configured, one cycle from connecting on
			// its own. Tearing its config down here would be actively harmful.
			name: "first synced-quiet observation only backs off",
			rows: syncedQuietCandidate(func(c *FleetConfigCandidate) {
				c.LastOutcome = outcomeAwaitingKey
			}),
			wantOutcome: outcomeSyncedQuiet,
		},
		{
			name: "grace period not elapsed only backs off",
			rows: syncedQuietCandidate(func(c *FleetConfigCandidate) {
				c.LastAttemptAt = forceNow.Add(-time.Minute)
			}),
			wantOutcome: outcomeSyncedQuiet,
		},
		{
			// THE ANTI-LOOP BOUND. A force already happened and no new pairing
			// epoch has opened since, so the car falls back to plain backoff
			// no matter how long it stays silent.
			name: "epoch budget already spent only backs off",
			rows: syncedQuietCandidate(func(c *FleetConfigCandidate) {
				c.ForcedRepushAt = forceNow.Add(-time.Hour)
			}),
			wantOutcome: outcomeSyncedQuiet,
		},
		{
			// ...and re-pairing re-arms it: the epoch moved past the last force.
			name: "a new pairing epoch re-arms the budget",
			rows: syncedQuietCandidate(func(c *FleetConfigCandidate) {
				c.ForcedRepushAt = forceNow.Add(-30 * time.Hour)
				c.SignedCommandAt = forceNow.Add(-10 * time.Minute)
			}),
			wantForced:  true,
			wantOutcome: outcomeForcedRepush,
		},
		{
			// A DEAD CAR. No command has ever applied, so the stamp cannot
			// vouch for it; Tesla says it is offline, so neither can the probe.
			name: "offline vehicle with no command history is never escalated",
			rows: syncedQuietCandidate(func(c *FleetConfigCandidate) {
				c.SignedCommandAt = time.Time{}
			}),
			setup:            func(h *forceHarness) { h.reader.vehicleState = "offline" },
			wantVehicleProbe: true,
			wantOutcome:      outcomeSyncedQuiet,
		},
		{
			// Same car, but Tesla reports it online: an owner who paired and
			// never sent a command still self-heals.
			name: "online vehicle with no command history is escalated on the probe",
			rows: syncedQuietCandidate(func(c *FleetConfigCandidate) {
				c.SignedCommandAt = time.Time{}
			}),
			setup:            func(h *forceHarness) { h.reader.vehicleState = "online" },
			wantForced:       true,
			wantVehicleProbe: true,
			wantOutcome:      outcomeForcedRepush,
		},
		{
			name: "asleep is not awake",
			rows: syncedQuietCandidate(func(c *FleetConfigCandidate) {
				c.SignedCommandAt = time.Time{}
			}),
			setup:            func(h *forceHarness) { h.reader.vehicleState = "asleep" },
			wantVehicleProbe: true,
			wantOutcome:      outcomeSyncedQuiet,
		},
		{
			name: "a failing awake probe does not escalate blind",
			rows: syncedQuietCandidate(func(c *FleetConfigCandidate) {
				c.SignedCommandAt = time.Time{}
			}),
			setup:            func(h *forceHarness) { h.reader.vehicleErr = errors.New("502 bad gateway") },
			wantVehicleProbe: true,
			wantOutcome:      outcomeSyncedQuiet,
		},
		{
			// A stale stamp does not get a free pass — it falls through to the
			// live probe like a car with no stamp at all.
			name: "a signed command outside the awake window falls back to the probe",
			rows: syncedQuietCandidate(func(c *FleetConfigCandidate) {
				c.SignedCommandAt = forceNow.Add(-72 * time.Hour)
			}),
			setup:            func(h *forceHarness) { h.reader.vehicleState = "offline" },
			wantVehicleProbe: true,
			wantOutcome:      outcomeSyncedQuiet,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newForceHarness(tt.rows, tt.setup)

			out, err := h.rec.Reconcile(context.Background())
			if err != nil {
				t.Fatalf("Reconcile: unexpected error: %v", err)
			}
			if out.AlreadySynced != 1 {
				t.Errorf("AlreadySynced = %d, want 1", out.AlreadySynced)
			}

			wantDeletes, wantPushes, wantForcedCount := 0, 0, 0
			if tt.wantForced {
				wantDeletes, wantPushes, wantForcedCount = 1, 1, 1
			}
			if h.writer.deleteCalls != wantDeletes {
				t.Errorf("delete calls = %d, want %d", h.writer.deleteCalls, wantDeletes)
			}
			if h.writer.calls != wantPushes {
				t.Errorf("push calls = %d, want %d", h.writer.calls, wantPushes)
			}
			if out.ForcedRepushes != wantForcedCount {
				t.Errorf("ForcedRepushes = %d, want %d", out.ForcedRepushes, wantForcedCount)
			}
			if tt.wantForced && len(h.writer.order) == 2 &&
				(h.writer.order[0] != "delete" || h.writer.order[1] != "push") {
				t.Errorf("write order = %v, want [delete push]", h.writer.order)
			}

			probes := 0
			if tt.wantVehicleProbe {
				probes = 1
			}
			if h.reader.vehicleCalls != probes {
				t.Errorf("GetVehicle calls = %d, want %d", h.reader.vehicleCalls, probes)
			}

			assertOutcome(t, h.attempts, tt.wantForced, tt.wantOutcome)
		})
	}
}

// assertOutcome checks that exactly one schedule write happened, on the right
// path (forced vs ordinary), with the right label.
func assertOutcome(t *testing.T, a *fakeAttemptRecorder, forced bool, want string) {
	t.Helper()
	got, other := a.recorded, a.forced
	if forced {
		got, other = a.forced, a.recorded
	}
	if len(other) != 0 {
		t.Errorf("unexpected writes on the other schedule path: %+v", other)
	}
	if len(got) != 1 {
		t.Fatalf("schedule writes = %d, want 1", len(got))
	}
	if got[0].outcome != want {
		t.Errorf("outcome = %q, want %q", got[0].outcome, want)
	}
	if len(a.cleared) != 0 {
		// A forced re-push must never clear the schedule — clearing is what
		// would let it fire again on the next pass and loop.
		t.Errorf("schedule was cleared (%v); the forced path must keep its backoff", a.cleared)
	}
}

// TestForcedRepushCreateFailureIsLoudAndRetriesSoon covers the one state this
// feature can leave behind that is worse than doing nothing: the DELETE landed
// and the POST did not, so the car now has no config at all.
func TestForcedRepushCreateFailureIsLoudAndRetriesSoon(t *testing.T) {
	h := newForceHarness(syncedQuietCandidate(nil), func(h *forceHarness) {
		h.writer.err = errors.New("503 service unavailable")
	})

	out, err := h.rec.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: unexpected error: %v", err)
	}
	if out.PushFailures != 1 || out.ForcedRepushes != 0 {
		t.Errorf("PushFailures=%d ForcedRepushes=%d, want 1 and 0", out.PushFailures, out.ForcedRepushes)
	}
	if len(h.attempts.forced) != 1 {
		t.Fatalf("forced schedule writes = %d, want 1 (the epoch budget must be stamped even on failure)",
			len(h.attempts.forced))
	}
	rec := h.attempts.forced[0]
	if rec.outcome != outcomeForcedRepushFail {
		t.Errorf("outcome = %q, want %q", rec.outcome, outcomeForcedRepushFail)
	}
	// Base interval, not the doubled backoff: the car may be unconfigured and
	// the ordinary push path is what fixes that.
	wantNext := forceNow.Add(defaultFleetConfigReconcileInterval)
	if !rec.next.Equal(wantNext) {
		t.Errorf("next attempt = %v, want %v (base interval)", rec.next, wantNext)
	}
}

// TestForcedRepushProceedsWhenDeleteFails: a 404 for a config that is already
// gone reads exactly like a transient delete error, and the POST is the part
// that matters, so the escalation continues.
func TestForcedRepushProceedsWhenDeleteFails(t *testing.T) {
	h := newForceHarness(syncedQuietCandidate(nil), func(h *forceHarness) {
		h.writer.deleteErr = errors.New("404 not found")
	})

	out, err := h.rec.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: unexpected error: %v", err)
	}
	if out.ForcedRepushes != 1 {
		t.Errorf("ForcedRepushes = %d, want 1", out.ForcedRepushes)
	}
	if h.writer.calls != 1 {
		t.Errorf("push calls = %d, want 1", h.writer.calls)
	}
}

// TestForcedRepushSkippedByTeslaCountsAsFailure: a 200 whose body skips the VIN
// is not a success — the same trap the whole reconciler exists for.
func TestForcedRepushSkippedByTeslaCountsAsFailure(t *testing.T) {
	h := newForceHarness(syncedQuietCandidate(nil), func(h *forceHarness) {
		h.writer.result = &FleetConfigResponse{Response: FleetConfigResult{
			SkippedVehicles: map[string]string{reconTestVIN: "missing_key"},
		}}
	})

	out, err := h.rec.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("Reconcile: unexpected error: %v", err)
	}
	if out.ForcedRepushes != 0 || out.PushFailures != 1 {
		t.Errorf("ForcedRepushes=%d PushFailures=%d, want 0 and 1", out.ForcedRepushes, out.PushFailures)
	}
	if len(h.attempts.forced) != 1 || h.attempts.forced[0].outcome != outcomeForcedRepushFail {
		t.Errorf("forced writes = %+v, want one %q", h.attempts.forced, outcomeForcedRepushFail)
	}
}

// TestAwaitingKeyBackoffIsCappedTighter pins the MYR-489 hole-2 mitigation: the
// state an owner is actively trying to leave must not inherit the 12-hour
// ceiling meant for unfixable cars.
func TestAwaitingKeyBackoffIsCappedTighter(t *testing.T) {
	cfg := FleetConfigReconcileConfig{}.withDefaults()

	tests := []struct {
		name    string
		outcome string
		attempt int
		want    time.Duration
	}{
		{"awaiting key deep in the curve is capped at 2h", outcomeAwaitingKey, 10, 2 * time.Hour},
		{"awaiting key early in the curve is untouched", outcomeAwaitingKey, 1, 30 * time.Minute},
		{"synced-quiet keeps the general 12h ceiling", outcomeSyncedQuiet, 10, 12 * time.Hour},
		{"read failure keeps the general 12h ceiling", outcomeReadFailed, 10, 12 * time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cfg.backoffForOutcome(tt.attempt, tt.outcome); got != tt.want {
				t.Errorf("backoffForOutcome(%d, %q) = %v, want %v", tt.attempt, tt.outcome, got, tt.want)
			}
		})
	}
}

// TestAwaitingKeyCapNeverInvertsTheCeilings: a misconfiguration must not make
// an unpairable car cost MORE than a dead one.
func TestAwaitingKeyCapNeverInvertsTheCeilings(t *testing.T) {
	cfg := FleetConfigReconcileConfig{
		Interval:              time.Minute,
		MaxBackoff:            10 * time.Minute,
		AwaitingKeyMaxBackoff: 5 * time.Hour,
	}.withDefaults()

	if cfg.AwaitingKeyMaxBackoff != cfg.MaxBackoff {
		t.Errorf("AwaitingKeyMaxBackoff = %v, want it clamped to MaxBackoff %v",
			cfg.AwaitingKeyMaxBackoff, cfg.MaxBackoff)
	}
}
