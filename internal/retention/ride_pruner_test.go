package retention

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// Ride retention sweep unit tests (MYR-447).
//
// Same shape as pruner_test.go: a scripted fake, an injectable clock, no
// sleeps. What is asserted here is PACE — convergence, budget, retries,
// shutdown — because scope lives in the store layer and is tested against a
// real database in internal/store/ride_pruner_test.go.

// fakeRideStore is a scripted backlog of two kinds of work: rides to delete and
// passenger records to scrub. It hands out batches of `batchSize` of each until
// both run out.
type fakeRideStore struct {
	mu sync.Mutex
	// remainingRides is the simulated backlog of deletable rides.
	remainingRides int
	// remainingScrubs is the simulated backlog of scrubbable passenger records.
	remainingScrubs int
	// calls counts PruneBatch invocations.
	calls int
	// failTimes is how many leading calls return an error before succeeding.
	failTimes int
	// err is what a failing call returns.
	err error
}

func (f *fakeRideStore) PruneBatch(ctx context.Context, batchSize int) (RideBatchOutcome, error) {
	f.mu.Lock()
	f.calls++
	shouldFail := f.failTimes > 0
	if shouldFail {
		f.failTimes--
	}
	f.mu.Unlock()

	if shouldFail {
		return RideBatchOutcome{}, f.err
	}
	if err := ctx.Err(); err != nil {
		return RideBatchOutcome{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	deleted := min(batchSize, f.remainingRides)
	f.remainingRides -= deleted
	scrubbed := min(batchSize, f.remainingScrubs)
	f.remainingScrubs -= scrubbed
	// Mirrors the real store: Exhausted only when BOTH claims come back empty,
	// never on a merely short batch.
	return RideBatchOutcome{
		RidesDeleted:       deleted,
		PassengersScrubbed: scrubbed,
		AuditRows:          1,
		Exhausted:          deleted == 0 && scrubbed == 0,
	}, nil
}

func (f *fakeRideStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// countingRideMetrics records what a pass reported.
type countingRideMetrics struct {
	deleted      int
	scrubbed     int
	batches      int
	batchErrors  int
	runs         int
	lastSuccess  time.Time
	successCalls int
}

func (m *countingRideMetrics) AddRidesDeleted(n int)            { m.deleted += n }
func (m *countingRideMetrics) AddPassengersScrubbed(n int)      { m.scrubbed += n }
func (m *countingRideMetrics) IncBatch()                        { m.batches++ }
func (m *countingRideMetrics) IncBatchError()                   { m.batchErrors++ }
func (m *countingRideMetrics) ObserveRunDuration(time.Duration) { m.runs++ }
func (m *countingRideMetrics) SetLastSuccess(t time.Time)       { m.lastSuccess = t; m.successCalls++ }

func newTestRidePruner(t *testing.T, store RideStore, cfg Config) (*RidePruner, *countingRideMetrics) {
	t.Helper()
	metrics := &countingRideMetrics{}
	cfg.Enabled = true
	return NewRidePruner(store, cfg, metrics, nil), metrics
}

// TestRideRunPassConvergesOverBatches is the batching proof: a backlog several
// times the batch size drains in ONE pass, in bounded steps, and the two kinds
// of work land on their own counters.
func TestRideRunPassConvergesOverBatches(t *testing.T) {
	store := &fakeRideStore{remainingRides: 250, remainingScrubs: 40}
	p, metrics := newTestRidePruner(t, store, Config{BatchSize: 100})

	p.RunPass(context.Background())

	if store.remainingRides != 0 {
		t.Errorf("ride backlog remaining = %d, want 0 — the pass did not converge", store.remainingRides)
	}
	if store.remainingScrubs != 0 {
		t.Errorf("scrub backlog remaining = %d, want 0", store.remainingScrubs)
	}
	// 250 rides at 100 a batch: 100, 100, 50, then ONE empty claim to confirm
	// the backlog is genuinely drained rather than merely unclaimable. The 40
	// scrubs are absorbed by the first batch and do not extend the pass.
	if got := store.callCount(); got != 4 {
		t.Errorf("PruneBatch calls = %d, want 4 (three batches + the confirming empty claim)", got)
	}
	if metrics.deleted != 250 {
		t.Errorf("metrics rides deleted = %d, want 250", metrics.deleted)
	}
	if metrics.scrubbed != 40 {
		t.Errorf("metrics passengers scrubbed = %d, want 40", metrics.scrubbed)
	}
	if metrics.batches != 4 {
		t.Errorf("metrics batches = %d, want 4", metrics.batches)
	}
	if metrics.successCalls != 1 {
		t.Errorf("last-success set %d times, want 1", metrics.successCalls)
	}
	if metrics.lastSuccess.IsZero() {
		t.Error("last-success timestamp is zero — the staleness alert would never clear")
	}
	if metrics.runs != 1 {
		t.Errorf("run duration observed %d times, want exactly 1 per pass", metrics.runs)
	}
}

// TestRideRunPassKeepsGoingForScrubOnlyWork is the property the shared engine
// could plausibly get wrong: a pass with NOTHING to delete but a scrub backlog
// must keep looping, because Exhausted is about both claims and the loop's row
// tally only counts deletions.
func TestRideRunPassKeepsGoingForScrubOnlyWork(t *testing.T) {
	store := &fakeRideStore{remainingRides: 0, remainingScrubs: 25}
	p, metrics := newTestRidePruner(t, store, Config{BatchSize: 10})

	p.RunPass(context.Background())

	if store.remainingScrubs != 0 {
		t.Errorf("scrub backlog remaining = %d, want 0 — a scrub-only pass must still converge",
			store.remainingScrubs)
	}
	if got := store.callCount(); got != 4 {
		t.Errorf("PruneBatch calls = %d, want 4 (10, 10, 5, then the confirming empty claim)", got)
	}
	if metrics.scrubbed != 25 {
		t.Errorf("metrics passengers scrubbed = %d, want 25", metrics.scrubbed)
	}
	if metrics.deleted != 0 {
		t.Errorf("metrics rides deleted = %d, want 0", metrics.deleted)
	}
	if metrics.successCalls != 1 {
		t.Errorf("last-success set %d times, want 1", metrics.successCalls)
	}
}

// TestRideRunPassRespectsBatchBudget: a backlog bigger than the budget stops at
// the budget rather than running until morning, and does not lose the remainder.
func TestRideRunPassRespectsBatchBudget(t *testing.T) {
	store := &fakeRideStore{remainingRides: 1000}
	p, metrics := newTestRidePruner(t, store, Config{BatchSize: 10, MaxBatchesPerPass: 5})

	p.RunPass(context.Background())

	if got := store.callCount(); got != 5 {
		t.Errorf("PruneBatch calls = %d, want 5 (the budget)", got)
	}
	if metrics.deleted != 50 {
		t.Errorf("metrics deleted = %d, want 50", metrics.deleted)
	}
	if store.remainingRides != 950 {
		t.Errorf("backlog remaining = %d, want 950 — the rest waits for the next pass", store.remainingRides)
	}
	// A capped pass is not a failed pass: it made progress, so the freshness
	// gauge must still move or an operator would page on a healthy backlog.
	if metrics.successCalls != 1 {
		t.Errorf("last-success set %d times on a capped pass, want 1", metrics.successCalls)
	}
	if metrics.batchErrors != 0 {
		t.Errorf("batch errors = %d, want 0", metrics.batchErrors)
	}
}

// TestRideRunPassRetriesThenEndsPass covers the retry ladder and the deliberate
// deviation from "skip to next batch".
func TestRideRunPassRetriesThenEndsPass(t *testing.T) {
	t.Run("a transient failure is retried and the pass continues", func(t *testing.T) {
		store := &fakeRideStore{remainingRides: 5, failTimes: 1, err: errors.New("boom")}
		p, metrics := newTestRidePruner(t, store, Config{BatchSize: 100})

		p.RunPass(context.Background())

		if store.remainingRides != 0 {
			t.Errorf("backlog remaining = %d, want 0 — the retry should have succeeded", store.remainingRides)
		}
		if metrics.batchErrors != 0 {
			t.Error("a batch that succeeded on retry must not count as a batch error")
		}
	})

	t.Run("exhausting the attempts ends the pass", func(t *testing.T) {
		store := &fakeRideStore{remainingRides: 500, failTimes: 99, err: errors.New("boom")}
		p, metrics := newTestRidePruner(t, store, Config{BatchSize: 10})

		p.RunPass(context.Background())

		if got := store.callCount(); got != pruneAttempts {
			t.Errorf("PruneBatch calls = %d, want %d (one batch's attempts)", got, pruneAttempts)
		}
		if metrics.batchErrors != 1 {
			t.Errorf("batch errors = %d, want 1", metrics.batchErrors)
		}
		// The pass failed, so the freshness gauge must NOT advance — that gauge
		// is the alert that catches a sweep silently failing every night.
		if metrics.successCalls != 0 {
			t.Errorf("last-success set %d times on a failed pass, want 0", metrics.successCalls)
		}
	})
}

// TestRideRunPassStopsOnCancellation is the shutdown contract: a pass in flight
// stops at the next batch boundary rather than draining the backlog.
func TestRideRunPassStopsOnCancellation(t *testing.T) {
	store := &fakeRideStore{remainingRides: 10000}
	p, metrics := newTestRidePruner(t, store, Config{BatchSize: 1, MaxBatchesPerPass: 10000})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for store.callCount() < 1 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()

	done := make(chan struct{})
	go func() { p.RunPass(ctx); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunPass did not stop after its context was cancelled")
	}

	if store.remainingRides == 0 {
		t.Error("pass drained the whole backlog despite cancellation")
	}
	// Cancellation is shutdown, not a fault.
	if metrics.batchErrors != 0 {
		t.Errorf("batch errors = %d on shutdown, want 0", metrics.batchErrors)
	}
	if metrics.successCalls != 0 {
		t.Errorf("last-success set %d times on an interrupted pass, want 0", metrics.successCalls)
	}
}

// TestRideRunStopsWhenDisabledOrUnwired: neither state may spin, and neither may
// panic. The unwired case is the one the shared engine makes easy to get wrong —
// binding a method value off a nil interface would produce a non-nil func.
func TestRideRunStopsWhenDisabledOrUnwired(t *testing.T) {
	tests := []struct {
		name  string
		store RideStore
		cfg   Config
	}{
		{"kill switch off", &fakeRideStore{}, Config{Enabled: false}},
		{"no store wired", nil, Config{Enabled: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := NewRidePruner(tc.store, tc.cfg, nil, nil)
			done := make(chan struct{})
			go func() { p.Run(context.Background()); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("Run did not return")
			}
		})
	}
}

// TestRideRunStopsOnContextCancel is the loop's shutdown assertion.
func TestRideRunStopsOnContextCancel(t *testing.T) {
	store := &fakeRideStore{}
	// A slot 12h away, so the timer never fires during the test.
	p, _ := newTestRidePruner(t, store, Config{})
	p.now = func() time.Time { return time.Date(2026, 8, 7, 15, 0, 0, 0, time.UTC) }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
	if store.callCount() != 0 {
		t.Error("Run ran a pass before its scheduled slot")
	}
}

// TestRideAndDriveSweepsShareTheEngineWithoutSharingState is the regression
// guard on the MYR-447 refactor: two sweeps built from the same generic engine
// must keep separate clocks, configs and metrics. A field accidentally hoisted
// to package scope would show up here as one pruner's pass moving the other's
// counters.
func TestRideAndDriveSweepsShareTheEngineWithoutSharingState(t *testing.T) {
	rideStore := &fakeRideStore{remainingRides: 5}
	driveStore := &fakeDriveStore{remaining: 7}
	ridePruner, rideMetrics := newTestRidePruner(t, rideStore, Config{BatchSize: 100})
	drivePruner, driveMetrics := newTestPruner(t, driveStore, Config{BatchSize: 100})

	ridePruner.RunPass(context.Background())

	if driveMetrics.batches != 0 || driveMetrics.deleted != 0 {
		t.Errorf("a ride pass moved the drive sweep's metrics: batches=%d deleted=%d",
			driveMetrics.batches, driveMetrics.deleted)
	}
	if driveStore.callCount() != 0 {
		t.Error("a ride pass called the drive store")
	}

	drivePruner.RunPass(context.Background())

	if rideMetrics.deleted != 5 {
		t.Errorf("ride metrics deleted = %d, want 5 — the drive pass must not have reset them", rideMetrics.deleted)
	}
	if driveMetrics.deleted != 7 {
		t.Errorf("drive metrics deleted = %d, want 7", driveMetrics.deleted)
	}

	// Separate clocks: moving one must not move the other's schedule.
	ridePruner.now = func() time.Time { return time.Date(2026, 8, 7, 1, 0, 0, 0, time.UTC) }
	rideWait := ridePruner.untilNextRun(ridePruner.now())
	driveWait := drivePruner.untilNextRun(time.Date(2026, 8, 7, 4, 0, 0, 0, time.UTC))
	if rideWait > 2*time.Hour+runJitter {
		t.Errorf("ride untilNextRun = %v, want ~2h from 01:00 UTC", rideWait)
	}
	if driveWait < 23*time.Hour {
		t.Errorf("drive untilNextRun = %v, want ~23h from 04:00 UTC", driveWait)
	}
}
