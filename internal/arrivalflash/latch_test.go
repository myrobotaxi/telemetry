package arrivalflash

import (
	"fmt"
	"sync"
	"testing"
)

// TestLatchClaim covers the whole contract of the exactly-once gate: the first
// claim for a key wins, every repeat loses, and distinct keys do not interfere.
func TestLatchClaim(t *testing.T) {
	tests := []struct {
		name  string
		size  int
		keys  []string
		wants []bool
	}{
		{
			name:  "first claim wins and the redelivery loses",
			size:  4,
			keys:  []string{"a", "a", "a"},
			wants: []bool{true, false, false},
		},
		{
			name:  "distinct keys are independent",
			size:  4,
			keys:  []string{"ride|pickup", "ride|dropoff", "ride|pickup"},
			wants: []bool{true, true, false},
		},
		{
			name:  "a full latch still admits new keys",
			size:  2,
			keys:  []string{"a", "b", "c"},
			wants: []bool{true, true, true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := newLatch(tt.size)
			for i, key := range tt.keys {
				if got := l.claim(key); got != tt.wants[i] {
					t.Errorf("claim(%q) #%d = %v, want %v", key, i+1, got, tt.wants[i])
				}
			}
		})
	}
}

// TestLatchEvictsTheOldestKey pins the bound and its exact cost. Overflowing
// the ring must forget the OLDEST key (so the map cannot grow without limit in
// a process that runs for months) and must keep the newest ones — and a
// redelivery of the forgotten key is admitted again, which is the documented
// price of the bound rather than a defect. It is bounded away in production by
// sizing the ring far above any plausible concurrent-ride count.
func TestLatchEvictsTheOldestKey(t *testing.T) {
	const size = 3
	l := newLatch(size)
	for i := 0; i < size; i++ {
		if !l.claim(fmt.Sprintf("k%d", i)) {
			t.Fatalf("first claim of k%d was refused", i)
		}
	}
	// One more than the ring holds: k0 falls out.
	if !l.claim("k3") {
		t.Fatal("claim of a new key on a full latch was refused")
	}
	// Checked BEFORE re-claiming k0: a refused claim does not insert, so these
	// are read-only, whereas admitting k0 again would evict k1 in its turn.
	for _, still := range []string{"k1", "k2", "k3"} {
		if l.claim(still) {
			t.Errorf("%s should still be latched", still)
		}
	}
	if !l.claim("k0") {
		t.Error("k0 should have been evicted by k3 and therefore admitted again")
	}
	if len(l.seen) != size {
		t.Errorf("latch holds %d keys, want at most %d", len(l.seen), size)
	}
}

// TestLatchIsSafeUnderConcurrentClaims pins the mutex. Production only ever
// claims from the bus's serial delivery goroutine, so this is about the type
// rather than the current caller — but "exactly one winner" is the property the
// whole feature rests on, and it must not quietly depend on a caller-side
// promise nothing enforces. Run with -race.
func TestLatchIsSafeUnderConcurrentClaims(t *testing.T) {
	const claimers = 64
	l := newLatch(defaultLatchSize)

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		wins int
	)
	start := make(chan struct{})
	for i := 0; i < claimers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if l.claim("ride|pickup") {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Errorf("%d goroutines won the claim, want exactly 1", wins)
	}
}
