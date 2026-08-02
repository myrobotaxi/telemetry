package push

import (
	"context"
)

// The Live Activity send path's worker accounting (MYR-398).
//
// Every push runs detached from the event bus, and Wait is how a shutdown —
// or a test — blocks until those detached workers are done. The counting is a
// mutex and a condition variable rather than the obvious sync.WaitGroup, and
// that is a correctness requirement, not taste.
//
// A WaitGroup's contract is that an Add which takes the counter off zero must
// happen-before Wait. This notifier cannot promise that. Wait's caller and the
// thing that starts workers are different goroutines with nothing ordering
// them: handleStatusChanged runs on the bus's delivery goroutine, so an event
// already accepted by Publish can still be in flight towards async at the
// moment shutdown calls Wait. cmd/telemetry-server never closes the bus before
// draining the notifier, and even a Close would not fix it — its drain has a
// timeout, and past that timeout delivery goroutines are still live.
//
// Held to a WaitGroup, that window is a silently empty counter: Wait sees zero,
// returns, and the process exits while the last transition's push was still on
// its way in. The race detector calls it out for the same reason (it was what
// broke main's -race build after #363), but the dropped push is the real cost —
// a rider's lock screen frozen on the previous state.
//
// A counter incremented under the same mutex the waiter blocks on has no such
// window: an increment either lands before Wait takes the lock, in which case
// Wait sees it and blocks, or after Wait has returned, in which case it belongs
// to the next drain. Cond.Wait releases mu while parked, so Subscribe and
// Unsubscribe keep working through a drain. And unlike a "closed" flag, this
// stays reusable: tests call Wait as a quiesce point over and over, and each
// call is just a barrier over whatever is in flight right then.

// Wait blocks until every in-flight update finishes.
func (a *ActivityNotifier) Wait() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for a.inflight > 0 {
		a.idle.Wait()
	}
}

// track registers work that runs on the CALLER's goroutine with the same drain
// async's workers use, and returns the function that retires it.
//
// The ticker's held-end pass (MYR-421) is why this exists. It reaches send —
// and the progress and phase-mark writes behind it — synchronously on the
// ticker goroutine, without ever passing through async, so until it was counted
// here Wait() returned at shutdown while those pushes and writes were still in
// flight. That is the same silently-empty-counter failure the comment above
// describes, arriving by a different door: not a WaitGroup racing an Add, but a
// whole code path the counter never knew about.
//
//	done := a.track()
//	defer done()
//
// No deadlock against Wait: Cond.Wait releases mu while parked, so an increment
// can land while a drain is in progress — and one that lands after Wait has
// returned belongs to the next drain, exactly as async's does.
func (a *ActivityNotifier) track() func() {
	a.mu.Lock()
	a.inflight++
	a.mu.Unlock()
	return a.workerDone
}

// async runs fn on a bounded worker with its own timeout, detached from the
// bus so a slow APNs round-trip never backs up event delivery.
func (a *ActivityNotifier) async(fn func(context.Context)) {
	a.mu.Lock()
	a.inflight++
	a.mu.Unlock()

	go func() {
		defer a.workerDone()
		a.sem <- struct{}{}
		defer func() { <-a.sem }()

		ctx, cancel := context.WithTimeout(context.Background(), a.cfg.Timeout)
		defer cancel()
		fn(ctx)
	}()
}

// workerDone retires one worker and wakes Wait when the last one leaves.
func (a *ActivityNotifier) workerDone() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.inflight--
	if a.inflight == 0 {
		a.idle.Broadcast()
	}
}
