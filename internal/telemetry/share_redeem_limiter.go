package telemetry

import (
	"sync"
	"time"
)

// redeemRateLimit is the per-user cap on invite-code redemption attempts.
//
// The code space is 36^6 ≈ 2.2 billion. At 10 attempts/minute a single account
// needs on the order of 400 years to cover 1% of it, which is what makes the
// short, human-readable code safe to hand out over a share sheet. Without this
// limit the code length would have to roughly double.
//
// The cap is per AUTHENTICATED USER, not per IP: redemption requires a valid
// token, so the account is the durable identity an attacker would have to churn,
// and IP-keying would penalise everyone behind one NAT while doing nothing
// against a botnet.
const (
	redeemRateLimit  = 10
	redeemRateWindow = time.Minute
)

// redeemLimiterSweepEvery bounds the limiter's memory: the sweep drops entries
// whose window has fully lapsed. Without it the map would retain one entry per
// account that has ever attempted a redemption.
const redeemLimiterSweepEvery = 5 * time.Minute

// redeemLimiter is a per-user fixed-window attempt counter.
//
// SINGLE-INSTANCE BY DESIGN, AND THAT IS A DOCUMENTED LIMITATION, NOT AN
// OVERSIGHT. The counter lives in this process's memory, so N machines would
// each allow the full budget and the effective cap would be N × the limit.
// fly.toml declares one [[vm]] with no scale-out for this app, so today the
// in-memory counter IS the global counter. If the deployment ever grows a
// second machine, this must move to a shared store (Postgres row or Redis)
// BEFORE the scale-out, not after — the failure mode is silent.
//
// Restarting the process clears the counters. That is acceptable: a deploy or
// crash gives an attacker one extra window, not an unbounded one, and the
// alternative (persisting counters) buys little against the same threat.
type redeemLimiter struct {
	limit  int
	window time.Duration

	mu        sync.Mutex
	attempts  map[string]*redeemWindow
	lastSweep time.Time
	now       func() time.Time // injectable for tests
}

// redeemWindow is one account's counter and the instant its window opened.
type redeemWindow struct {
	count     int
	windowTop time.Time
}

// newRedeemLimiter builds a limiter with the given cap and window. A limit of
// zero or less disables limiting — used only by tests that are exercising
// something else.
func newRedeemLimiter(limit int, window time.Duration) *redeemLimiter { //nolint:unparam // window varies in tests
	return &redeemLimiter{
		limit:    limit,
		window:   window,
		attempts: make(map[string]*redeemWindow),
		now:      time.Now,
	}
}

// allow records an attempt for userID and reports whether it may proceed.
//
// EVERY attempt is counted, including the ones that will succeed. Counting only
// failures would let an attacker with one valid code interleave it with guesses
// and never trip the limit.
func (l *redeemLimiter) allow(userID string) bool {
	if l.limit <= 0 {
		return true
	}

	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweepLocked(now)

	entry, ok := l.attempts[userID]
	if !ok || now.Sub(entry.windowTop) >= l.window {
		l.attempts[userID] = &redeemWindow{count: 1, windowTop: now}
		return true
	}
	if entry.count >= l.limit {
		return false
	}
	entry.count++
	return true
}

// sweepLocked drops fully-lapsed entries. Caller must hold the mutex.
func (l *redeemLimiter) sweepLocked(now time.Time) {
	if now.Sub(l.lastSweep) < redeemLimiterSweepEvery {
		return
	}
	l.lastSweep = now
	for userID, entry := range l.attempts {
		if now.Sub(entry.windowTop) >= l.window {
			delete(l.attempts, userID)
		}
	}
}
