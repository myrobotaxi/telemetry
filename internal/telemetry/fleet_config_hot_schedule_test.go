package telemetry

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// hotReconciler builds a reconciler whose clock the test drives, so a multi-pass
// timeline can be walked minute by minute rather than slept through.
func hotReconciler(
	clock *time.Time,
	reader *fakeConfigReader,
	writer *fakeConfigWriter,
	attempts *fakeAttemptRecorder,
) *FleetConfigReconciler {
	r := NewFleetConfigReconciler(
		FleetConfigReconcilerDeps{
			Candidates: &fakeCandidateLister{},
			Attempts:   attempts,
			Reader:     reader,
			Writer:     writer,
			Tokens:     okToken(),
			Pairing:    &fakePairingResetter{},
		},
		FleetConfigReconcileConfig{},
		EndpointConfig{Hostname: "telemetry.myrobotaxi.app", Port: 443},
		slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	)
	r.now = func() time.Time { return *clock }
	return r
}

// TestJoeyVerronesTimeline is MYR-529 end to end on the row that produced it.
//
// The car linked at 01:22:08Z, proved its key at 01:23:10Z, and Tesla reported
// the config synced on every look. Before this change the sequence was: examine
// once, decline (first synced-quiet observation), wait fifteen minutes, and —
// because the "Vehicle" row kept being written by ordinary web traffic — never
// be listed as a candidate again. The owner drove with no telemetry.
//
// The assertion is on the WHOLE ladder, not on any one gate, because the fix is
// the shape of the schedule rather than a single predicate: decline at the
// epoch, decline at +1, force at +3.
func TestJoeyVerronesTimeline(t *testing.T) {
	epoch := time.Date(2026, 8, 10, 1, 23, 10, 0, time.UTC)
	clock := epoch

	reader := &fakeConfigReader{synced: true}
	writer := &fakeConfigWriter{result: &FleetConfigResponse{Response: FleetConfigResult{UpdatedVehicles: 1}}}
	attempts := &fakeAttemptRecorder{}
	r := hotReconciler(&clock, reader, writer, attempts)

	// The row as prod held it the instant the pairing signal landed: seeded at
	// link time (so last_outcome is '' and attempt_count 0), epoch just stamped,
	// offline, and with a "lastUpdated" far too fresh for the staleness rule.
	c := FleetConfigCandidate{
		VehicleID:       "veh-joey",
		VIN:             reconTestVIN,
		UserID:          "user-joey",
		Status:          "offline",
		LastUpdated:     epoch.Add(-time.Minute),
		SignedCommandAt: epoch,
	}

	// Pass 0 — the epoch instant. Declined: a car one second past pairing gets
	// a cycle to connect on its own.
	var out ReconcileOutcome
	r.reconcileOne(context.Background(), c, &out)
	if len(attempts.forced) != 0 {
		t.Fatalf("pass 0 forced a re-push; want the first observation declined")
	}
	if len(attempts.recorded) != 1 || attempts.recorded[0].outcome != outcomeSyncedQuiet {
		t.Fatalf("pass 0 recorded %+v, want one %q", attempts.recorded, outcomeSyncedQuiet)
	}
	if got, want := attempts.recorded[0].next, epoch.Add(hotPairingLadder[0]); !got.Equal(want) {
		t.Fatalf("pass 0 next_attempt = %v, want %v (the hot ladder, not +15m)", got, want)
	}

	// Pass 1 — one minute later, exactly as the ladder scheduled. Still
	// declined, but now on the hot gate rather than the fifteen-minute one.
	clock = epoch.Add(hotPairingLadder[0])
	c.AttemptCount = 1
	c.LastOutcome = outcomeSyncedQuiet
	c.LastAttemptAt = epoch
	r.reconcileOne(context.Background(), c, &out)
	if len(attempts.forced) != 0 {
		t.Fatalf("pass 1 forced a re-push; want the ladder to give the car one more pass")
	}
	if got, want := attempts.recorded[1].next, clock.Add(hotPairingLadder[1]); !got.Equal(want) {
		t.Fatalf("pass 1 next_attempt = %v, want %v", got, want)
	}

	// Pass 2 — three minutes past pairing. THE FIX: the forced delete-and-create
	// is spent here, while the owner is still standing at the car, instead of
	// never.
	clock = epoch.Add(hotPairingLadder[0] + hotPairingLadder[1])
	c.AttemptCount = 2
	c.LastAttemptAt = epoch.Add(hotPairingLadder[0])
	r.reconcileOne(context.Background(), c, &out)

	if writer.deleteCalls != 1 || writer.calls != 1 {
		t.Fatalf("pass 2: delete=%d push=%d, want 1 and 1 (delete-then-create)",
			writer.deleteCalls, writer.calls)
	}
	if len(attempts.forced) != 1 || attempts.forced[0].outcome != outcomeForcedRepush {
		t.Fatalf("pass 2 forced writes = %+v, want one %q", attempts.forced, outcomeForcedRepush)
	}
	if reader.vehicleCalls != 0 {
		t.Errorf("GetVehicle called %d times; the epoch stamp already proves the car awake", reader.vehicleCalls)
	}
	// Three minutes, end to end. The whole point of the issue.
	if elapsed := clock.Sub(epoch); elapsed > 5*time.Minute {
		t.Errorf("pairing to forced re-push took %v; want minutes, not an interval", elapsed)
	}
}

// TestHotEpochNeverDelaysAnAlreadyEligibleCar guards the direction the hot gate
// must NOT move. Nabil's car — synced, silent for two days, last examined eight
// hours ago — satisfies the wall-clock grace outright, and a fresh epoch
// resetting its attempt_count to zero must not make it wait for the ladder.
func TestHotEpochNeverDelaysAnAlreadyEligibleCar(t *testing.T) {
	clock := forceNow
	writer := &fakeConfigWriter{result: &FleetConfigResponse{Response: FleetConfigResult{UpdatedVehicles: 1}}}
	attempts := &fakeAttemptRecorder{}
	r := hotReconciler(&clock, &fakeConfigReader{synced: true}, writer, attempts)

	var out ReconcileOutcome
	r.reconcileOne(context.Background(), FleetConfigCandidate{
		VehicleID:       "veh-nabil",
		VIN:             reconTestVIN,
		UserID:          "user-nabil",
		Status:          "offline",
		LastUpdated:     forceNow.Add(-48 * time.Hour),
		AttemptCount:    0, // just reset by the pairing epoch
		LastOutcome:     outcomeSyncedQuiet,
		LastAttemptAt:   forceNow.Add(-8 * time.Hour),
		SignedCommandAt: forceNow,
	}, &out)

	if out.ForcedRepushes != 1 {
		t.Fatalf("forced re-pushes = %d, want 1 — the wall-clock grace was already satisfied", out.ForcedRepushes)
	}
}

// TestStreamingCandidateCostsNothing is the bound that makes the hot ladder
// affordable: a car that came up on its own is resolved from its own row.
func TestStreamingCandidateCostsNothing(t *testing.T) {
	clock := forceNow
	reader := &fakeConfigReader{synced: true}
	writer := &fakeConfigWriter{}
	attempts := &fakeAttemptRecorder{}
	r := hotReconciler(&clock, reader, writer, attempts)

	var out ReconcileOutcome
	r.reconcileOne(context.Background(), FleetConfigCandidate{
		VehicleID:       "veh-james",
		VIN:             reconTestVIN,
		UserID:          "user-james",
		Status:          "parked",
		LastUpdated:     forceNow.Add(-20 * time.Second),
		SignedCommandAt: forceNow,
	}, &out)

	if reader.calls != 0 || writer.calls != 0 || writer.deleteCalls != 0 {
		t.Errorf("Tesla was called (%d read, %d push, %d delete); a streaming row answers for free",
			reader.calls, writer.calls, writer.deleteCalls)
	}
	if out.AlreadyStreaming != 1 {
		t.Errorf("AlreadyStreaming = %d, want 1", out.AlreadyStreaming)
	}
	if len(attempts.cleared) != 1 || attempts.cleared[0] != "veh-james" {
		t.Errorf("cleared = %v, want the remaining hot passes cancelled for veh-james", attempts.cleared)
	}
	if len(attempts.recorded) != 0 {
		t.Errorf("recorded %+v; a streaming car must not accumulate attempts", attempts.recorded)
	}
}

// TestOfflineStatusIsNeverMistakenForStreaming pins the half of the free check
// that actually mattered for Joey: his "Vehicle" row was written at 01:41:33 by
// ordinary web traffic while the car had never streamed a frame. Freshness
// alone would have cancelled his passes and left him dark forever.
func TestOfflineStatusIsNeverMistakenForStreaming(t *testing.T) {
	clock := forceNow
	reader := &fakeConfigReader{synced: true}
	r := hotReconciler(&clock, reader, &fakeConfigWriter{}, &fakeAttemptRecorder{})

	var out ReconcileOutcome
	r.reconcileOne(context.Background(), FleetConfigCandidate{
		VehicleID:   "veh-joey",
		VIN:         reconTestVIN,
		UserID:      "user-joey",
		Status:      "offline",
		LastUpdated: forceNow.Add(-5 * time.Second),
	}, &out)

	if out.AlreadyStreaming != 0 {
		t.Fatalf("AlreadyStreaming = %d; a fresh row with the schema-default status has never streamed", out.AlreadyStreaming)
	}
	if reader.calls != 1 {
		t.Errorf("config reads = %d, want 1 — the car must still be examined", reader.calls)
	}
}

// TestNextAttemptGap covers the ladder's arithmetic, including the decay back
// into the MYR-448 curve and the total absence of any hot behaviour outside an
// epoch.
func TestNextAttemptGap(t *testing.T) {
	clock := forceNow
	r := hotReconciler(&clock, &fakeConfigReader{}, &fakeConfigWriter{}, &fakeAttemptRecorder{})
	base := r.cfg.Interval

	tests := []struct {
		name    string
		c       FleetConfigCandidate
		outcome string
		want    time.Duration
	}{
		{
			name:    "no epoch at all keeps the ordinary first interval",
			c:       FleetConfigCandidate{},
			outcome: outcomeSyncedQuiet,
			want:    base,
		},
		{
			name:    "a stale epoch is not hot",
			c:       FleetConfigCandidate{SignedCommandAt: forceNow.Add(-2 * defaultFleetConfigHotEpochWindow), AttemptCount: 1},
			outcome: outcomeSyncedQuiet,
			want:    base * 2,
		},
		{
			name:    "hot rung 0",
			c:       FleetConfigCandidate{SignedCommandAt: forceNow},
			outcome: outcomeSyncedQuiet,
			want:    hotPairingLadder[0],
		},
		{
			name:    "hot rung 2",
			c:       FleetConfigCandidate{SignedCommandAt: forceNow, AttemptCount: 2},
			outcome: outcomeSyncedQuiet,
			want:    hotPairingLadder[2],
		},
		{
			name:    "past the ladder, decayed to the base interval rather than jumped to a doubled one",
			c:       FleetConfigCandidate{SignedCommandAt: forceNow, AttemptCount: len(hotPairingLadder)},
			outcome: outcomeSyncedQuiet,
			want:    base,
		},
		{
			name:    "one past that resumes doubling",
			c:       FleetConfigCandidate{SignedCommandAt: forceNow, AttemptCount: len(hotPairingLadder) + 1},
			outcome: outcomeSyncedQuiet,
			want:    base * 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.nextAttemptGap(tt.c, tt.outcome); got != tt.want {
				t.Errorf("nextAttemptGap = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestTickIntervalIsNotTheBackoffBase is the MYR-529 loop-cadence finding as an
// assertion: a due row must be noticed in about a minute, and the tick may
// never be the thing that decides a car's real gap.
func TestTickIntervalIsNotTheBackoffBase(t *testing.T) {
	cfg := FleetConfigReconcileConfig{}.withDefaults()

	if cfg.TickInterval != defaultFleetConfigTickInterval {
		t.Errorf("TickInterval = %v, want %v", cfg.TickInterval, defaultFleetConfigTickInterval)
	}
	if cfg.TickInterval >= cfg.Interval {
		t.Errorf("TickInterval %v >= Interval %v — a due row would wait a whole backoff to be seen",
			cfg.TickInterval, cfg.Interval)
	}
	if cfg.TickInterval > time.Minute {
		t.Errorf("TickInterval = %v; Joey's row must not be able to sit 2+ minutes past next_attempt", cfg.TickInterval)
	}

	// A misconfiguration that ticks slower than the backoff base is the same
	// bug with a different number in it, and is clamped rather than honoured.
	clamped := FleetConfigReconcileConfig{Interval: 5 * time.Minute, TickInterval: time.Hour}.withDefaults()
	if clamped.TickInterval != 5*time.Minute {
		t.Errorf("clamped TickInterval = %v, want it pulled down to Interval", clamped.TickInterval)
	}
}

// TestReconcilePassPassesTheHotWindow proves the pass actually asks for the
// exemption — without it the ladder above schedules examinations that the
// candidate query would never return.
func TestReconcilePassPassesTheHotWindow(t *testing.T) {
	lister := &fakeCandidateLister{}
	r := NewFleetConfigReconciler(
		FleetConfigReconcilerDeps{
			Candidates: lister,
			Attempts:   &fakeAttemptRecorder{},
			Reader:     &fakeConfigReader{},
			Writer:     &fakeConfigWriter{},
			Tokens:     okToken(),
			Pairing:    &fakePairingResetter{},
		},
		FleetConfigReconcileConfig{},
		EndpointConfig{Hostname: "telemetry.myrobotaxi.app", Port: 443},
		slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	)
	r.now = func() time.Time { return forceNow }

	if _, err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if want := forceNow.Add(-defaultFleetConfigHotEpochWindow); !lister.gotHotSince.Equal(want) {
		t.Errorf("hotSince = %v, want %v", lister.gotHotSince, want)
	}
}
