package arrivalflash

import "sync"

// latch is the exactly-once gate, keyed by (ride, waypoint): the first claim
// for a key wins, every later one is refused.
//
// It is belt-and-braces. The detector publishes `ride.waypoint_arrived` exactly
// once per waypoint already, so in a healthy process this refuses nothing. It
// exists because the failure it guards against is one a person standing on the
// pavement can SEE — a car flashing six times looks broken, not welcoming — and
// because a redelivery is exactly the kind of thing a future bus change, a
// replay, or a second publisher would introduce quietly.
//
// It is deliberately IN-MEMORY, not persisted. The latch's job is to
// de-duplicate one event within one process lifetime; a restart forgetting it
// is the right outcome, because a restart also means nothing is mid-greeting
// and a car that never got its flashes will not get them now either (the
// arrival that would have triggered them is long committed).
//
// # The bound
//
// A map keyed by ride id in a process that runs for months is an unbounded map,
// so the latch is a fixed-size ring: inserting one key past its size evicts the
// OLDEST. What that costs at the edge is precise and tiny — a redelivery of an
// event that is 512 waypoints old would flash a car whose ride ended long ago —
// and it is bounded away by sizing the ring far above any plausible number of
// concurrent rides (see defaultLatchSize). The alternative, TTL eviction, buys
// nothing here: it needs a clock and a sweep to defend against the same
// vanishing case.
type latch struct {
	mu   sync.Mutex
	seen map[string]struct{}
	// ring holds the keys in insertion order; the empty string marks a slot
	// that has never been used.
	ring []string
	next int
}

// newLatch builds a latch holding at most size keys. size is assumed positive
// (Config.withDefaults guarantees it).
func newLatch(size int) *latch {
	return &latch{
		seen: make(map[string]struct{}, size),
		ring: make([]string, size),
	}
}

// claim records key and reports whether THIS call was the first to do so.
//
// It is mutex-guarded even though production only ever calls it from the bus's
// serial per-subscriber goroutine: the cost is a few nanoseconds on a path that
// runs once per arrival, and the alternative is a type whose safety depends on
// a caller-side property nothing enforces.
func (l *latch) claim(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if _, ok := l.seen[key]; ok {
		return false
	}
	if evicted := l.ring[l.next]; evicted != "" {
		delete(l.seen, evicted)
	}
	l.ring[l.next] = key
	l.next = (l.next + 1) % len(l.ring)
	l.seen[key] = struct{}{}
	return true
}
