package identity

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// ipRateLimiter is a per-client-IP token-bucket limiter guarding the
// pre-authentication /api/auth/* surface against brute-force / credential
// spraying. It follows the in-repo precedent of golang.org/x/time/rate
// (internal/geocode). Idle IP entries are evicted to bound memory.
type ipRateLimiter struct {
	mu       sync.Mutex
	limiters map[string]*ipEntry
	limit    rate.Limit
	burst    int
	ttl      time.Duration
	now      func() time.Time
}

type ipEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// newIPRateLimiter builds a limiter allowing perMinute sustained requests per
// IP with the given burst. perMinute<=0 disables limiting (allow always).
func newIPRateLimiter(perMinute, burst int) *ipRateLimiter {
	if burst < 1 {
		burst = 1
	}
	return &ipRateLimiter{
		limiters: make(map[string]*ipEntry),
		limit:    rate.Limit(float64(perMinute) / 60.0),
		burst:    burst,
		ttl:      10 * time.Minute,
		now:      time.Now,
	}
}

// allow reports whether a request from ip may proceed. A disabled limiter
// (non-positive rate) always allows.
func (l *ipRateLimiter) allow(ip string) bool {
	if l.limit <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	e, ok := l.limiters[ip]
	if !ok {
		e = &ipEntry{limiter: rate.NewLimiter(l.limit, l.burst)}
		l.limiters[ip] = e
	}
	e.lastSeen = now
	l.evictLocked(now)
	return e.limiter.AllowN(now, 1)
}

// evictLocked drops entries idle longer than ttl. Called under l.mu; cheap
// because the pre-auth IP set is small at v1 scale.
func (l *ipRateLimiter) evictLocked(now time.Time) {
	for ip, e := range l.limiters {
		if now.Sub(e.lastSeen) > l.ttl {
			delete(l.limiters, ip)
		}
	}
}
