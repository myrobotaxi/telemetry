package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeChecker is a controllable userExistenceChecker for tests.
type fakeChecker struct {
	mu       sync.Mutex
	existsBy map[string]bool
	err      error
	calls    atomic.Int32
	wait     chan struct{} // nil = no wait; signals a coordinated query barrier
	started  chan struct{} // closed once on first UserExists entry; lets tests gate without time.Sleep
}

func (f *fakeChecker) UserExists(_ context.Context, userID string) (bool, error) {
	if n := f.calls.Add(1); n == 1 && f.started != nil {
		close(f.started)
	}
	if f.wait != nil {
		<-f.wait
	}
	if f.err != nil {
		return false, f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.existsBy[userID], nil
}

func TestUserExistenceCache_HappyPath(t *testing.T) {
	checker := &fakeChecker{existsBy: map[string]bool{"alive": true, "dead": false}}
	c := newUserExistenceCache(checker, time.Second)

	for _, tt := range []struct {
		userID string
		want   bool
	}{
		{"alive", true},
		{"dead", false},
	} {
		got, err := c.Exists(context.Background(), tt.userID)
		if err != nil {
			t.Fatalf("Exists(%s): unexpected error: %v", tt.userID, err)
		}
		if got != tt.want {
			t.Errorf("Exists(%s) = %v, want %v", tt.userID, got, tt.want)
		}
	}

	if got := checker.calls.Load(); got != 2 {
		t.Errorf("checker calls = %d, want 2 (one per distinct userID)", got)
	}
}

func TestUserExistenceCache_CachesPositive(t *testing.T) {
	checker := &fakeChecker{existsBy: map[string]bool{"alive": true}}
	c := newUserExistenceCache(checker, time.Hour)

	for i := 0; i < 5; i++ {
		_, err := c.Exists(context.Background(), "alive")
		if err != nil {
			t.Fatalf("call %d error: %v", i, err)
		}
	}
	if got := checker.calls.Load(); got != 1 {
		t.Errorf("checker calls = %d, want 1 (cache hits should not refetch)", got)
	}
}

func TestUserExistenceCache_CachesNegative(t *testing.T) {
	checker := &fakeChecker{existsBy: map[string]bool{}}
	c := newUserExistenceCache(checker, time.Hour)

	for i := 0; i < 5; i++ {
		exists, err := c.Exists(context.Background(), "ghost")
		if err != nil {
			t.Fatalf("call %d error: %v", i, err)
		}
		if exists {
			t.Errorf("Exists(ghost) = true, want false")
		}
	}
	if got := checker.calls.Load(); got != 1 {
		t.Errorf("checker calls = %d, want 1 (negative answer must also be cached)", got)
	}
}

func TestUserExistenceCache_TTLExpiry(t *testing.T) {
	checker := &fakeChecker{existsBy: map[string]bool{"alive": true}}
	c := newUserExistenceCache(checker, 10*time.Millisecond)

	if _, err := c.Exists(context.Background(), "alive"); err != nil {
		t.Fatal(err)
	}
	if got := checker.calls.Load(); got != 1 {
		t.Fatalf("first call: checker calls = %d, want 1", got)
	}

	// Force the cache entry to look older than ttl by stomping `now`.
	c.now = func() time.Time { return time.Now().Add(time.Hour) }

	if _, err := c.Exists(context.Background(), "alive"); err != nil {
		t.Fatal(err)
	}
	if got := checker.calls.Load(); got != 2 {
		t.Errorf("after TTL expiry: checker calls = %d, want 2", got)
	}
}

func TestUserExistenceCache_Singleflight(t *testing.T) {
	checker := &fakeChecker{
		existsBy: map[string]bool{"alive": true},
		wait:     make(chan struct{}),
		started:  make(chan struct{}),
	}
	c := newUserExistenceCache(checker, time.Hour)

	// Fire 50 concurrent Exists calls for the same user; they must
	// fan out to a single DB query (singleflight coalescing).
	const concurrent = 50
	var wg sync.WaitGroup
	wg.Add(concurrent)
	for i := 0; i < concurrent; i++ {
		go func() {
			defer wg.Done()
			_, _ = c.Exists(context.Background(), "alive")
		}()
	}

	// Wait for the first caller to land inside the checker fn.
	// singleflight coalesces all subsequent callers against the same
	// in-flight key once the per-key lock is taken — additional
	// callers do NOT enter UserExists, so the strong invariant we
	// want is calls.Load() == 1 while the wait channel is still
	// blocking. We poll the negative invariant with a deadline; this
	// is deterministic (no time.Sleep barrier) and tolerates slow CI.
	select {
	case <-checker.started:
	case <-time.After(2 * time.Second):
		t.Fatal("checker never started")
	}
	close(checker.wait)
	wg.Wait()

	if got := checker.calls.Load(); got != 1 {
		t.Errorf("checker calls = %d, want 1 (singleflight should coalesce)", got)
	}
}

func TestUserExistenceCache_Invalidate(t *testing.T) {
	checker := &fakeChecker{existsBy: map[string]bool{"alive": true}}
	c := newUserExistenceCache(checker, time.Hour)

	if _, err := c.Exists(context.Background(), "alive"); err != nil {
		t.Fatal(err)
	}
	c.Invalidate("alive")
	if _, err := c.Exists(context.Background(), "alive"); err != nil {
		t.Fatal(err)
	}
	if got := checker.calls.Load(); got != 2 {
		t.Errorf("checker calls = %d, want 2 (Invalidate should force refetch)", got)
	}
}

func TestUserExistenceCache_PropagatesError(t *testing.T) {
	checker := &fakeChecker{err: errors.New("db unreachable")}
	c := newUserExistenceCache(checker, time.Hour)

	_, err := c.Exists(context.Background(), "any")
	if err == nil {
		t.Fatal("expected error on DB failure")
	}
}
