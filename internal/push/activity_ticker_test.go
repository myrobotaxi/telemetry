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

	// MYR-573 — 24–36s, superseding MYR-194's 60–90s window: with ticks
	// finally DELIVERED (immediate priority under the frequent-updates
	// budget), the cadence is what the rider sees, and once a minute reads as
	// a stalled card beside the in-app surface.
	const (
		wantMin = 24 * time.Second
		wantMax = 36 * time.Second
	)
	// The jittered range is a closed-form property of the two constants; assert
	// it directly so a future edit to either is caught even if the sampling
	// below happens to miss the edges.
	spread := time.Duration(float64(cfg.Interval) * cfg.JitterFraction)
	if lo := cfg.Interval - spread; lo < wantMin {
		t.Errorf("minimum cadence = %s, want >= %s (MYR-573)", lo, wantMin)
	}
	if hi := cfg.Interval + spread; hi > wantMax {
		t.Errorf("maximum cadence = %s, want <= %s (MYR-573)", hi, wantMax)
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
	if cfg.Interval != 30*time.Second {
		t.Errorf("Interval = %s, want 30s (MYR-573's delivered cadence)", cfg.Interval)
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

// TestTickerPassSendsImmediatePriorityUpdates is the ordinary pass.
//
// ⚠️ THE PRIORITY ASSERTION INVERTED WITH MYR-573, and the history matters
// enough to keep here: this test used to REQUIRE LowPriority, pinning MYR-194
// decision 3 — and the field showed priority-5 Activity updates deferred
// indefinitely on a locked phone, so the pinned behaviour WAS the client's
// "card only updates when I open the app". A tick that does not arrive is not
// a tick. Immediate priority is budgeted by the widget extension's
// `NSSupportsLiveActivitiesFrequentUpdates` declaration (MYR-573's iOS half).
func TestTickerPassSendsImmediatePriorityUpdates(t *testing.T) {
	ticker, sender, _ := newTestTicker(t, 3, nil)

	ticker.RunPass(context.Background())

	sent := sender.Sent()
	if len(sent) != 3 {
		t.Fatalf("sent %d updates, want one per active leg (3)", len(sent))
	}
	for i, s := range sent {
		if s.LowPriority {
			t.Errorf("update %d sent at conserving priority; a deferred tick never reaches a locked phone (MYR-573)", i)
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

// TestTickerCapRotatesRatherThanStarving is the MYR-172 review fix.
//
// The LIST orders by `updated_at ASC` and its own comment calls that
// anti-starvation: when a pass is capped, the cap is supposed to shed the
// Activities that were refreshed most recently. That was FALSE, because nothing
// ever moved updated_at after registration — the order was a fixed permutation
// and every capped pass shed the same tail forever. With two rides and a cap of
// one, the second ride's Activity would never have been updated again for the
// whole length of its ride.
//
// Two passes at cap=1 must therefore reach two DIFFERENT Activities.
func TestTickerCapRotatesRatherThanStarving(t *testing.T) {
	ticker, sender, store := newTestTicker(t, 2, nil)
	ticker.cfg.MaxPerPass = 1

	ticker.RunPass(context.Background())
	ticker.RunPass(context.Background())

	sent := sender.Sent()
	if len(sent) != 2 {
		t.Fatalf("sent %d updates over two capped passes, want 2", len(sent))
	}
	if sent[0].ActivityToken == sent[1].ActivityToken {
		t.Errorf("both passes went to %s — the cap is starving the second Activity,"+
			" which is exactly what ordering by updated_at is supposed to prevent",
			tokenPrefix(sent[0].ActivityToken))
	}

	// And the rotation is driven by a real write, not by luck in the fake:
	// each pass must have stamped exactly the row it delivered to.
	pushed := store.pushedKeys()
	if len(pushed) != 2 {
		t.Fatalf("MarkPushed recorded %d keys, want 2 (one per pass)", len(pushed))
	}
	if pushed[0] == pushed[1] {
		t.Errorf("both passes stamped %+v; the second pass re-picked the first row", pushed[0])
	}
}

// TestTickerMarksOnlyDeliveredActivities pins the other half: a row Apple
// refused is not "recently pushed". Stamping it anyway would let a permanently
// failing Activity hold the front of the queue and starve healthy ones — the
// same bug, inverted.
func TestTickerMarksOnlyDeliveredActivities(t *testing.T) {
	ticker, sender, store := newTestTicker(t, 3, nil)
	sender.Err = errors.New("apns unavailable")

	ticker.RunPass(context.Background())

	if got := store.pushedKeys(); len(got) != 0 {
		t.Errorf("MarkPushed recorded %d keys after every send failed, want 0", len(got))
	}
}

// TestTickerSurvivesAMarkFailure — a failed stamp costs one pass of FAIRNESS,
// never a pass of updates. The sends have already happened by then.
func TestTickerSurvivesAMarkFailure(t *testing.T) {
	ticker, sender, store := newTestTicker(t, 2, nil)
	store.markErr = errors.New("db blip")

	ticker.RunPass(context.Background())

	if got := len(sender.Sent()); got != 2 {
		t.Errorf("sent %d updates despite a mark-pushed failure, want 2", got)
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

// TestTickerDisabledRefreshesNothing pins the kill-switch. Lifecycle updates
// carry on; only the periodic refresh stops, and the Activity's own stale-date
// then does exactly the job MYR-194 designed it for.
//
// The switch no longer stops the LOOP (MYR-421) — the held completion `end`
// runs on it and is a lifecycle transition rather than a refresh — so this now
// drives a pass directly instead of relying on Run returning at once. The
// held-end half of that boundary is TestHeldEndRunsWithTheEtaTickerOff.
func TestTickerDisabledRefreshesNothing(t *testing.T) {
	ticker, sender, _ := newTestTicker(t, 2, nil)
	ticker.cfg.Enabled = false

	ticker.RunPass(context.Background())

	if got := len(sender.Sent()); got != 0 {
		t.Errorf("a disabled ticker sent %d ETA refreshes, want 0", got)
	}
}

// TestTickerRunStopsWhenUnwired — a ticker with no store or no notifier has
// nothing to do at all, and must not spin a timer forever proving it.
func TestTickerRunStopsWhenUnwired(t *testing.T) {
	ticker := NewActivityTicker(nil, nil, TickerConfig{Enabled: true}, discardLogger())

	done := make(chan struct{})
	go func() {
		ticker.Run(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return with nothing wired")
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
