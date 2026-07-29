package telemetry

import (
	"sync"
	"testing"
	"time"
)

// TestRedeemLimiter is the unit-level contract of the brute-force guard behind
// POST /api/invites/redeem. The 6-character code space is only 36^6, so this
// limiter is not a politeness measure — it is the reason the code can be short
// enough to read aloud.
func TestRedeemLimiter(t *testing.T) {
	t.Run("allows exactly the budget, then refuses", func(t *testing.T) {
		l := newRedeemLimiter(3, time.Minute)
		for i := 1; i <= 3; i++ {
			if !l.allow("user-a") {
				t.Fatalf("attempt %d was refused while under the cap", i)
			}
		}
		if l.allow("user-a") {
			t.Error("attempt 4 was allowed; the cap is 3")
		}
	})

	t.Run("the budget is per user", func(t *testing.T) {
		l := newRedeemLimiter(2, time.Minute)
		for i := 0; i < 2; i++ {
			l.allow("user-a")
		}
		if l.allow("user-a") {
			t.Fatal("user-a is not capped")
		}
		// One exhausted account must not lock out everybody else.
		if !l.allow("user-b") {
			t.Error("user-b was refused because user-a exhausted their own budget")
		}
	})

	t.Run("the window rolls over", func(t *testing.T) {
		now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
		l := newRedeemLimiter(2, time.Minute)
		l.now = func() time.Time { return now }

		l.allow("user-a")
		l.allow("user-a")
		if l.allow("user-a") {
			t.Fatal("third attempt inside the window was allowed")
		}

		now = now.Add(time.Minute + time.Second)
		if !l.allow("user-a") {
			t.Error("the budget did not reset after the window elapsed")
		}
	})

	t.Run("a non-positive limit disables limiting", func(t *testing.T) {
		l := newRedeemLimiter(0, time.Minute)
		for i := 0; i < 100; i++ {
			if !l.allow("user-a") {
				t.Fatalf("attempt %d refused by a disabled limiter", i)
			}
		}
	})

	t.Run("the sweep drops lapsed entries without dropping live ones", func(t *testing.T) {
		now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
		l := newRedeemLimiter(2, time.Minute)
		l.now = func() time.Time { return now }

		l.allow("stale-user")

		// Jump past both the window and the sweep interval, then touch the
		// limiter so the sweep runs.
		now = now.Add(redeemLimiterSweepEvery + time.Minute)
		l.allow("fresh-user")

		l.mu.Lock()
		_, staleKept := l.attempts["stale-user"]
		_, freshKept := l.attempts["fresh-user"]
		l.mu.Unlock()

		if staleKept {
			t.Error("a fully-lapsed entry survived the sweep; the map grows without bound")
		}
		if !freshKept {
			t.Error("the sweep dropped a live entry, which would hand back a fresh budget")
		}
	})

	t.Run("is safe under concurrent use", func(t *testing.T) {
		const goroutines, each = 8, 50
		// Budget = exactly one more than the concurrent attempts, so the
		// assertions below pin the count to the exact value: one left, then
		// none.
		l := newRedeemLimiter(goroutines*each+1, time.Minute)

		var wg sync.WaitGroup
		for g := 0; g < goroutines; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < each; i++ {
					l.allow("user-a")
				}
			}()
		}
		wg.Wait()

		// Every attempt was inside the budget, so exactly one more is left.
		if !l.allow("user-a") {
			t.Error("the counter over-counted under concurrency")
		}
		if l.allow("user-a") {
			t.Error("the counter under-counted under concurrency — a race here means the cap is not a cap")
		}
	})
}
