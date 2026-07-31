package push

import (
	"context"
	"errors"
	"testing"
	"time"
)

// newTestTicker wires a ticker over n live legs on distinct rides.
func newTestTicker(t *testing.T, legs int, prefs PrefStore) (*ActivityTicker, *FakeActivitySender, *fakeActivityStore) {
	t.Helper()
	n, sender, store := newTestActivityNotifier(t, prefs)

	store.legs = nil
	for i := range legs {
		store.legs = append(store.legs, ActivityLeg{
			Activity: Activity{
				RideRequestID: string(rune('a'+i)) + "_ride",
				UserID:        testRiderID,
				Token:         string(rune('a'+i)) + "_token",
			},
			RideContext: RideContext{
				Status:      "enroute",
				VehicleName: "Blue Whale",
				Destination: "Home",
				ETAMinutes:  intPtr(4),
			},
		})
	}

	ticker := NewActivityTicker(n, store, TickerConfig{Enabled: true}, discardLogger())
	return ticker, sender, store
}

// TestTickerCadenceLandsInTheSpecifiedWindow is the scheduling assertion that
// matters: MYR-194 says 60–90s, and the default interval and jitter were chosen
// together so the ACTUAL wait lands inside that window at both extremes rather
// than merely averaging it.
func TestTickerCadenceLandsInTheSpecifiedWindow(t *testing.T) {
	cfg := TickerConfig{}.withDefaults()

	const (
		wantMin = 60 * time.Second
		wantMax = 90 * time.Second
	)
	// The jittered range is a closed-form property of the two constants; assert
	// it directly so a future edit to either is caught even if the sampling
	// below happens to miss the edges.
	spread := time.Duration(float64(cfg.Interval) * cfg.JitterFraction)
	if lo := cfg.Interval - spread; lo < wantMin {
		t.Errorf("minimum cadence = %s, want >= %s (MYR-194)", lo, wantMin)
	}
	if hi := cfg.Interval + spread; hi > wantMax {
		t.Errorf("maximum cadence = %s, want <= %s (MYR-194)", hi, wantMax)
	}

	for range 2000 {
		got := jitterDuration(cfg.Interval, cfg.JitterFraction)
		if got < wantMin || got > wantMax {
			t.Fatalf("jitterDuration produced %s, outside the %s–%s window", got, wantMin, wantMax)
		}
	}
}

// TestJitterDurationDegradesSafely pins the two inputs that must not produce a
// zero or negative timer — a zero wait would spin the loop.
func TestJitterDurationDegradesSafely(t *testing.T) {
	if got := jitterDuration(0, 0.2); got != 0 {
		t.Errorf("jitterDuration(0) = %s, want 0", got)
	}
	if got := jitterDuration(30*time.Second, 0); got != 30*time.Second {
		t.Errorf("jitterDuration with no jitter = %s, want the interval unchanged", got)
	}
}

// TestTickerDefaults pins every default, since all six are operational
// decisions rather than arbitrary numbers.
func TestTickerDefaults(t *testing.T) {
	cfg := TickerConfig{}.withDefaults()
	if cfg.Interval != 75*time.Second {
		t.Errorf("Interval = %s, want 75s (midpoint of the MYR-194 window)", cfg.Interval)
	}
	if cfg.JitterFraction != 0.20 {
		t.Errorf("JitterFraction = %v, want 0.20", cfg.JitterFraction)
	}
	if cfg.SweepAge != 24*time.Hour {
		t.Errorf("SweepAge = %s, want 24h", cfg.SweepAge)
	}
	if cfg.MaxPerPass <= 0 || cfg.StartupDelay <= 0 || cfg.ListTimeout <= 0 {
		t.Errorf("zero-valued defaults survived withDefaults: %+v", cfg)
	}
}

// TestTickerPassSendsConservingPriorityUpdates is the ordinary pass.
func TestTickerPassSendsConservingPriorityUpdates(t *testing.T) {
	ticker, sender, _ := newTestTicker(t, 3, nil)

	ticker.RunPass(context.Background())

	sent := sender.Sent()
	if len(sent) != 3 {
		t.Fatalf("sent %d updates, want one per active leg (3)", len(sent))
	}
	for i, s := range sent {
		if !s.LowPriority {
			t.Errorf("update %d sent at immediate priority; ETA ticks must not compete with lifecycle events for Apple's budget", i)
		}
		if s.Event != ActivityEventUpdate {
			t.Errorf("update %d event = %q, want update — a tick never ends an Activity", i, s.Event)
		}
		if s.DismissalDate != nil {
			t.Errorf("update %d carries a dismissal-date", i)
		}
		if s.ContentState.ETA == nil {
			t.Errorf("update %d carries no ETA despite a known nav ETA", i)
		}
	}
}

// TestTickerPassRefreshesTheStaleDate is why the ticker exists at all: the
// content may be identical to last tick, but the stale-date moves, which is
// what keeps the Activity out of its own "as of X min ago" rendering.
func TestTickerPassRefreshesTheStaleDate(t *testing.T) {
	ticker, sender, _ := newTestTicker(t, 1, nil)

	ticker.RunPass(context.Background())

	sent := sender.Sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d updates, want 1", len(sent))
	}
	if got, want := sent[0].StaleDate(), fixedNow.Add(StaleAfter); !got.Equal(want) {
		t.Errorf("stale-date = %s, want %s", got, want)
	}
}

// TestTickerPassRespectsTheCap pins the per-pass bound. An Activity shed by the
// cap is re-listed next tick, and because the LIST is ordered least-recently-
// updated first, the cap sheds the freshest rows rather than starving the
// hungriest.
func TestTickerPassRespectsTheCap(t *testing.T) {
	ticker, sender, _ := newTestTicker(t, 5, nil)
	ticker.cfg.MaxPerPass = 2

	ticker.RunPass(context.Background())

	if got := len(sender.Sent()); got != 2 {
		t.Errorf("sent %d updates with MaxPerPass=2, want 2", got)
	}
}

// TestTickerPassHonoursTheMuteGate proves the ETA ticker goes through the same
// preference gate as the lifecycle path — a rider who muted ride updates must
// not be reached 60 times an hour by the surface that ignores the switch.
func TestTickerPassHonoursTheMuteGate(t *testing.T) {
	prefs := newFakePrefStore()
	muted := DefaultPrefs()
	muted.RideLifecycle = false
	prefs.byUser[testRiderID] = muted

	ticker, sender, _ := newTestTicker(t, 3, prefs)
	ticker.RunPass(context.Background())

	if got := len(sender.Sent()); got != 0 {
		t.Errorf("sent %d ETA ticks to a muted rider, want 0", got)
	}
}

// TestTickerPassSurvivesAListFailure — a database blip costs one pass, not the
// loop.
func TestTickerPassSurvivesAListFailure(t *testing.T) {
	ticker, sender, store := newTestTicker(t, 2, nil)
	store.listErr = errors.New("pool exhausted")

	ticker.RunPass(context.Background())

	if got := len(sender.Sent()); got != 0 {
		t.Errorf("sent %d updates after a failed list, want 0", got)
	}
}

// TestTickerSweepRunsHourlyNotEveryTick pins the sweep cadence. At the 75s tick
// rate an unconditional sweep would be ~1,150 pointless DELETEs a day.
func TestTickerSweepRunsHourlyNotEveryTick(t *testing.T) {
	ticker, _, store := newTestTicker(t, 1, nil)

	// First pass sweeps (lastSweep is zero).
	ticker.RunPass(context.Background())
	if store.swept != 24*time.Hour {
		t.Fatalf("first pass swept with age %s, want 24h", store.swept)
	}

	// A second pass one tick later must NOT sweep again.
	store.swept = 0
	ticker.notifier.now = func() time.Time { return fixedNow.Add(75 * time.Second) }
	ticker.RunPass(context.Background())
	if store.swept != 0 {
		t.Error("swept again 75s after the previous sweep; the sweep is hourly")
	}

	// An hour on, it sweeps.
	ticker.notifier.now = func() time.Time { return fixedNow.Add(sweepEvery + time.Second) }
	ticker.RunPass(context.Background())
	if store.swept != 24*time.Hour {
		t.Error("did not sweep an hour after the previous sweep")
	}
}

// TestTickerDisabledDoesNothing pins the kill-switch. Lifecycle updates carry
// on; only the periodic refresh stops, and the Activity's own stale-date then
// does exactly the job MYR-194 designed it for.
func TestTickerDisabledDoesNothing(t *testing.T) {
	ticker, sender, _ := newTestTicker(t, 2, nil)
	ticker.cfg.Enabled = false

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	ticker.Run(ctx) // returns immediately rather than blocking on the timer

	if got := len(sender.Sent()); got != 0 {
		t.Errorf("a disabled ticker sent %d updates, want 0", got)
	}
}

// TestTickerRunStopsOnContextCancel proves the loop is cancellable, which is
// the whole of its shutdown contract.
func TestTickerRunStopsOnContextCancel(t *testing.T) {
	ticker, _, _ := newTestTicker(t, 1, nil)
	ticker.cfg.StartupDelay = time.Hour // never fires

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		ticker.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}
