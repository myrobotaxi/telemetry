package identity

import (
	"testing"
	"time"
)

func TestIPRateLimiter_BurstThenThrottle(t *testing.T) {
	l := newIPRateLimiter(60, 2) // 1/sec, burst 2
	base := time.Unix(1_000_000, 0)
	l.now = func() time.Time { return base }

	if !l.allow("1.1.1.1") {
		t.Fatal("first request in burst should be allowed")
	}
	if !l.allow("1.1.1.1") {
		t.Fatal("second request in burst should be allowed")
	}
	if l.allow("1.1.1.1") {
		t.Fatal("third immediate request should be throttled")
	}
	// A different IP has its own bucket.
	if !l.allow("2.2.2.2") {
		t.Fatal("distinct IP should be allowed")
	}
	// After time passes, the bucket refills.
	l.now = func() time.Time { return base.Add(2 * time.Second) }
	if !l.allow("1.1.1.1") {
		t.Fatal("bucket should refill after 2s")
	}
}

func TestIPRateLimiter_DisabledAlwaysAllows(t *testing.T) {
	l := newIPRateLimiter(0, 0) // disabled
	for i := 0; i < 100; i++ {
		if !l.allow("9.9.9.9") {
			t.Fatal("disabled limiter must always allow")
		}
	}
}

func TestIPRateLimiter_Eviction(t *testing.T) {
	l := newIPRateLimiter(60, 1)
	base := time.Unix(2_000_000, 0)
	l.now = func() time.Time { return base }
	l.allow("a")
	l.allow("b")
	// Advance beyond ttl; a new request prunes idle entries.
	l.now = func() time.Time { return base.Add(l.ttl + time.Minute) }
	l.allow("c")
	l.mu.Lock()
	_, aStill := l.limiters["a"]
	l.mu.Unlock()
	if aStill {
		t.Error("idle entry should have been evicted")
	}
}
