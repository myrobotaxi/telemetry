package drain

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestGroupWaitCoversWorkRegisteredConcurrently is the whole reason this
// package is not a sync.WaitGroup (MYR-398, MYR-410).
//
// Wait always runs on a different goroutine from the one registering work — in
// production the bus's delivery goroutine registers while shutdown waits, stood
// in for here — and nothing orders the two. So a unit of work may be registered
// at the very moment a drain begins, and Wait must still cover it. A WaitGroup
// did not: its counter may be read empty in that window, which is a shutdown
// that returns before the last push, and -race reports it as a data race.
func TestGroupWaitCoversWorkRegisteredConcurrently(t *testing.T) {
	var g Group

	const units = 200
	var completed atomic.Int64
	registered := make(chan struct{})
	go func() {
		defer close(registered)
		for i := 0; i < units; i++ {
			done := g.Track()
			go func() {
				defer done()
				completed.Add(1)
			}()
		}
	}()

	// Drain repeatedly against the live producer, the way shutdown races it.
	for draining := true; draining; {
		g.Wait()
		select {
		case <-registered:
			draining = false
		default:
		}
	}

	// Everything is registered by now, so this drain is the whole set.
	g.Wait()
	if got := completed.Load(); got != units {
		t.Errorf("Wait() returned with %d of %d units finished — a drain that ends early drops work that was already accepted", got, units)
	}
}

// TestGroupWaitOnIdleGroupReturns pins the zero value as usable: a Group nobody
// has tracked on must not block, and must not nil-panic on its lazily built
// condition variable.
func TestGroupWaitOnIdleGroupReturns(t *testing.T) {
	var g Group
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.Wait()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait() blocked on a Group with no work in flight")
	}
}

// TestGroupWaitIsReusable pins Wait as a repeatable barrier rather than a
// one-way latch — tests lean on it as a quiesce point over and over, and a
// shutdown may drain more than once.
func TestGroupWaitIsReusable(t *testing.T) {
	var g Group
	for round := 0; round < 3; round++ {
		var ran atomic.Bool
		done := g.Track()
		go func() {
			defer done()
			ran.Store(true)
		}()
		g.Wait()
		if !ran.Load() {
			t.Fatalf("round %d: Wait() returned before the tracked work ran", round)
		}
	}
}

// TestGroupRetireIsIdempotent pins the guard on a double retire. Track hands
// its retire function to arbitrary callers and, unlike sync.WaitGroup, there is
// no panic backstop here — so a second call must be a no-op rather than drive
// the counter below zero, which would leave every LATER drain returning while
// work was still live.
func TestGroupRetireIsIdempotent(t *testing.T) {
	var g Group

	done := g.Track()
	done()
	done()

	// A fresh unit of work must still hold the next drain open.
	var finished atomic.Bool
	retire := g.Track()
	go func() {
		time.Sleep(25 * time.Millisecond)
		finished.Store(true)
		retire()
	}()

	g.Wait()
	if !finished.Load() {
		t.Error("Wait() returned while work was in flight — the double retire drove the counter below zero")
	}
}
