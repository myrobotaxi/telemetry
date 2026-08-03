// Package drain counts detached work so a shutdown can block until it lands.
//
// Every consumer of it has the same shape: an events.Bus handler runs on the
// bus's delivery goroutine, hands its work to a bounded worker, and returns
// immediately; a Wait on some OTHER goroutine — process shutdown, or a test
// using it as a quiesce point — has to cover whatever is in flight. The
// counting is a mutex and a condition variable rather than the obvious
// sync.WaitGroup, and that is a correctness requirement, not taste.
//
// A WaitGroup's contract is that an Add which takes the counter off zero must
// happen-before Wait. None of these consumers can promise that. Wait's caller
// and the thing that starts workers are different goroutines with nothing
// ordering them: the handler runs on the bus's delivery goroutine, so an event
// already accepted by Publish can still be in flight towards the Add at the
// moment shutdown calls Wait. cmd/telemetry-server never closes the bus before
// draining its consumers, and even a Close would not fix it — the bus's own
// drain has a timeout, and past that timeout delivery goroutines are still
// live.
//
// Held to a WaitGroup, that window is a silently empty counter: Wait sees zero,
// returns, and the process exits while the last event's work was still on its
// way in. The race detector calls it out for the same reason (it is what broke
// main's -race build after MYR-398), but the dropped work is the real cost — a
// push that never reaches a rider's lock screen, a nav pickup that never
// reaches the car, on every deploy that lands mid-ride.
//
// A counter incremented under the same mutex the waiter blocks on has no such
// window: an increment either lands before Wait takes the lock, in which case
// Wait sees it and blocks, or after Wait has returned, in which case it belongs
// to the next drain. Cond.Wait releases the mutex while parked, so a drain in
// progress never freezes the counting itself. And unlike a "closed" flag, this
// stays reusable: tests call Wait over and over, and each call is just a
// barrier over whatever is in flight right then.
//
// # What this does NOT do
//
// A Group drains work that has been STARTED. It cannot drain work nobody has
// told it about, and on the event-bus path there is a whole category of that:
// subscriber channels are BUFFERED, so an event Publish has already accepted
// can be sitting in a buffer with its handler never entered. Track has not run,
// the counter is zero, and Wait returns immediately — byte-for-byte the
// WaitGroup outcome above. The mutex serialises the counter; it cannot order
// work that is not yet countable.
//
// So the guarantee is a PAIR, and neither half is one alone:
//
//	bus.Close(ctx)  // runs each buffered backlog through its handler,
//	                // turning accepted work into started work
//	group.Wait()    // then covers the workers those handlers started
//
// Reversed, or with the Close missing, the drain reads an empty counter and the
// process exits with the backlog undelivered. "Belongs to the next drain" is
// fine mid-flight and worthless at shutdown, where there is no next drain. See
// cmd/telemetry-server's shutdown-order block for the sequence, and
// internal/push's shutdown_drain_test.go for the pinned behaviour.
//
// The pattern was proven on the Live Activity notifier in MYR-398 and lifted
// into this package in MYR-410, when the alert notifier (internal/push) and the
// nav dispatcher (internal/dispatch) turned out to carry the identical latent
// bug and a third hand-rolled copy stopped being defensible.
package drain

import "sync"

// Group is a set of in-flight work items drained together. The zero value is
// ready to use; it must not be copied after first use (go vet's copylocks
// enforces that).
//
// Register work with Track BEFORE handing it to its goroutine. Counting on the
// CALLER's goroutine is the entire point — it leaves no window in which the
// work exists but is uncounted:
//
//	done := g.Track()
//	go func() {
//		defer done()
//		...
//	}()
//
// Track equally covers work that runs on the caller's own goroutine (the
// ticker's held-end pass in MYR-421 is one such path), which a
// WaitGroup-around-a-go-statement idiom tends to miss entirely.
type Group struct {
	mu       sync.Mutex
	idle     *sync.Cond // lazily built under mu; nil means nobody has ever parked
	inflight int
}

// Track registers one unit of work and returns the function that retires it.
//
// The returned function may be called from any goroutine, and retires the work
// only once however many times it is called. That idempotence is deliberate: a
// double retire would drive the counter below zero and leave EVERY later Wait
// returning while work was still live — precisely the failure this package
// exists to prevent — and unlike sync.WaitGroup there is no panic here to catch
// it.
func (g *Group) Track() func() {
	g.mu.Lock()
	g.inflight++
	g.mu.Unlock()

	var once sync.Once
	return func() { once.Do(g.retire) }
}

// Wait blocks until every unit of work registered by Track has been retired.
//
// It is a barrier over what is in flight at the moment it is called, not a
// one-way latch: work registered after Wait returns belongs to the next drain.
// Safe to call repeatedly and from several goroutines at once.
func (g *Group) Wait() {
	g.mu.Lock()
	defer g.mu.Unlock()
	for g.inflight > 0 {
		g.cond().Wait()
	}
}

// retire drops one unit of work and wakes Wait when the last one leaves.
func (g *Group) retire() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.inflight--
	if g.inflight == 0 && g.idle != nil {
		g.idle.Broadcast()
	}
}

// cond returns the condition variable, building it on the first park. The
// caller must hold mu — which is also why retire can skip a nil idle: a waiter
// builds the cond before parking, under the same mutex retire takes, so nil
// means there is nobody to wake.
func (g *Group) cond() *sync.Cond {
	if g.idle == nil {
		g.idle = sync.NewCond(&g.mu)
	}
	return g.idle
}
