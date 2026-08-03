package retention

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeDriveStore is a scripted backlog: it hands out batches of `batchSize`
// until `remaining` runs out, so a pass's convergence can be asserted without a
// database.
type fakeDriveStore struct {
	mu sync.Mutex
	// remaining is the simulated backlog of eligible drives.
	remaining int
	// calls counts PruneBatch invocations.
	calls int
	// failTimes is how many leading calls return an error before succeeding.
	failTimes int
	// err is what a failing call returns.
	err error
	// blockUntil, when non-nil, is closed by the test to release a call.
	blockUntil chan struct{}
}

func (f *fakeDriveStore) PruneBatch(ctx context.Context, batchSize int) (BatchOutcome, error) {
	f.mu.Lock()
	f.calls++
	shouldFail := f.failTimes > 0
	if shouldFail {
		f.failTimes--
	}
	block := f.blockUntil
	f.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return BatchOutcome{}, ctx.Err()
		}
	}
	if shouldFail {
		return BatchOutcome{}, f.err
	}
	if err := ctx.Err(); err != nil {
		return BatchOutcome{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	deleted := min(batchSize, f.remaining)
	f.remaining -= deleted
	return BatchOutcome{
		DrivesDeleted: deleted,
		AuditRows:     1,
		Exhausted:     deleted < batchSize,
	}, nil
}

func (f *fakeDriveStore) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// countingMetrics records what a pass reported.
type countingMetrics struct {
	deleted      int
	batches      int
	batchErrors  int
	runs         int
	lastSuccess  time.Time
	successCalls int
}

func (m *countingMetrics) AddDrivesDeleted(n int)           { m.deleted += n }
func (m *countingMetrics) IncBatch()                        { m.batches++ }
func (m *countingMetrics) IncBatchError()                   { m.batchErrors++ }
func (m *countingMetrics) ObserveRunDuration(time.Duration) { m.runs++ }
func (m *countingMetrics) SetLastSuccess(t time.Time)       { m.lastSuccess = t; m.successCalls++ }

func newTestPruner(t *testing.T, store DriveStore, cfg Config) (*Pruner, *countingMetrics) {
	t.Helper()
	metrics := &countingMetrics{}
	cfg.Enabled = true
	p := NewPruner(store, cfg, metrics, nil)
	return p, metrics
}

// TestRunPassConvergesOverBatches is the batching proof: a backlog several
// times the batch size drains in ONE pass, in bounded steps.
func TestRunPassConvergesOverBatches(t *testing.T) {
	store := &fakeDriveStore{remaining: 250}
	p, metrics := newTestPruner(t, store, Config{BatchSize: 100})

	p.RunPass(context.Background())

	if store.remaining != 0 {
		t.Errorf("backlog remaining = %d, want 0 — the pass did not converge", store.remaining)
	}
	// 250 at 100 a batch: 100, 100, 50. The short batch terminates the loop, so
	// convergence costs no extra empty call.
	if got := store.callCount(); got != 3 {
		t.Errorf("PruneBatch calls = %d, want 3", got)
	}
	if metrics.deleted != 250 {
		t.Errorf("metrics deleted = %d, want 250", metrics.deleted)
	}
	if metrics.batches != 3 {
		t.Errorf("metrics batches = %d, want 3", metrics.batches)
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

// TestRunPassRespectsBatchBudget covers the first-production-run case: a
// backlog bigger than the budget stops at the budget rather than running until
// morning, and does not lose the remainder.
func TestRunPassRespectsBatchBudget(t *testing.T) {
	store := &fakeDriveStore{remaining: 1000}
	p, metrics := newTestPruner(t, store, Config{BatchSize: 10, MaxBatchesPerPass: 5})

	p.RunPass(context.Background())

	if got := store.callCount(); got != 5 {
		t.Errorf("PruneBatch calls = %d, want 5 (the budget)", got)
	}
	if metrics.deleted != 50 {
		t.Errorf("metrics deleted = %d, want 50", metrics.deleted)
	}
	if store.remaining != 950 {
		t.Errorf("backlog remaining = %d, want 950 — the rest waits for the next pass", store.remaining)
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

// TestRunPassRetriesThenEndsPass covers §5.6's retry ladder and the deliberate
// deviation from "skip to next batch".
func TestRunPassRetriesThenEndsPass(t *testing.T) {
	t.Run("a transient failure is retried and the pass continues", func(t *testing.T) {
		store := &fakeDriveStore{remaining: 5, failTimes: 1, err: errors.New("boom")}
		p, metrics := newTestPruner(t, store, Config{BatchSize: 100})

		p.RunPass(context.Background())

		if store.remaining != 0 {
			t.Errorf("backlog remaining = %d, want 0 — the retry should have succeeded", store.remaining)
		}
		if metrics.batchErrors != 0 {
			t.Error("a batch that succeeded on retry must not count as a batch error")
		}
	})

	t.Run("exhausting the attempts ends the pass", func(t *testing.T) {
		store := &fakeDriveStore{remaining: 500, failTimes: 99, err: errors.New("boom")}
		p, metrics := newTestPruner(t, store, Config{BatchSize: 10})

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

// TestRunPassStopsOnCancellation is the shutdown contract: a pass in flight
// stops at the next batch boundary rather than draining the backlog.
func TestRunPassStopsOnCancellation(t *testing.T) {
	store := &fakeDriveStore{remaining: 10000}
	p, metrics := newTestPruner(t, store, Config{BatchSize: 1, MaxBatchesPerPass: 10000})

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after the first batch commits, then let the pass notice.
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

	if store.remaining == 0 {
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

// TestRunStopsOnContextCancel is the loop's shutdown assertion — the same shape
// the LA ticker uses.
func TestRunStopsOnContextCancel(t *testing.T) {
	store := &fakeDriveStore{}
	// A slot 12h away, so the timer never fires during the test.
	p, _ := newTestPruner(t, store, Config{})
	p.now = func() time.Time { return time.Date(2026, 8, 2, 15, 0, 0, 0, time.UTC) }

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

// TestRunStopsWhenDisabledOrUnwired: neither state may spin.
func TestRunStopsWhenDisabledOrUnwired(t *testing.T) {
	tests := []struct {
		name  string
		store DriveStore
		cfg   Config
	}{
		{"kill switch off", &fakeDriveStore{}, Config{Enabled: false}},
		{"no store wired", nil, Config{Enabled: true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := NewPruner(tc.store, tc.cfg, nil, nil)
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

// TestUntilNextRunTargetsTheDailySlot pins the schedule arithmetic (§5.2).
func TestUntilNextRunTargetsTheDailySlot(t *testing.T) {
	p, _ := newTestPruner(t, &fakeDriveStore{}, Config{})

	tests := []struct {
		name    string
		now     time.Time
		wantMin time.Duration
		wantMax time.Duration
	}{
		{
			name:    "before the slot, same day",
			now:     time.Date(2026, 8, 2, 1, 0, 0, 0, time.UTC),
			wantMin: 2 * time.Hour,
			wantMax: 2*time.Hour + runJitter,
		},
		{
			name:    "after the slot, rolls to tomorrow",
			now:     time.Date(2026, 8, 2, 4, 0, 0, 0, time.UTC),
			wantMin: 23 * time.Hour,
			wantMax: 23*time.Hour + runJitter,
		},
		{
			name:    "exactly on the slot, rolls to tomorrow rather than firing twice",
			now:     time.Date(2026, 8, 2, 3, 0, 0, 0, time.UTC),
			wantMin: 24 * time.Hour,
			wantMax: 24*time.Hour + runJitter,
		},
		{
			name: "a non-UTC clock is normalised, not taken at face value",
			// 01:00+05:00 is 2026-08-01 20:00 UTC, so the next 03:00 UTC slot is
			// seven hours out — NOT the two hours a naive read of the wall clock
			// would give.
			now:     time.Date(2026, 8, 2, 1, 0, 0, 0, time.FixedZone("UTC+5", 5*3600)),
			wantMin: 7 * time.Hour,
			wantMax: 7*time.Hour + runJitter,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := p.untilNextRun(tc.now)
			if got < tc.wantMin || got > tc.wantMax {
				t.Errorf("untilNextRun = %v, want within [%v, %v]", got, tc.wantMin, tc.wantMax)
			}
		})
	}
}

// TestConfigDefaults guards the pace knobs against a zero-value Config.
func TestConfigDefaults(t *testing.T) {
	got := Config{}.withDefaults()
	if got.RunAtUTC != defaultRunAtUTC {
		t.Errorf("RunAtUTC = %v, want %v", got.RunAtUTC, defaultRunAtUTC)
	}
	if got.BatchSize != defaultBatchSize {
		t.Errorf("BatchSize = %d, want %d", got.BatchSize, defaultBatchSize)
	}
	if got.MaxBatchesPerPass != defaultMaxBatchesPerPass {
		t.Errorf("MaxBatchesPerPass = %d, want %d", got.MaxBatchesPerPass, defaultMaxBatchesPerPass)
	}
	if got.BatchTimeout != defaultBatchTimeout {
		t.Errorf("BatchTimeout = %v, want %v", got.BatchTimeout, defaultBatchTimeout)
	}
}
