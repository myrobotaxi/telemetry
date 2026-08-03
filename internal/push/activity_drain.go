package push

import (
	"context"
	"fmt"
)

// The Live Activity send path's worker accounting (MYR-398, MYR-410).
//
// Every push runs detached from the event bus, and Wait is how a shutdown — or
// a test — blocks until those detached workers are done. The counting lives in
// internal/drain rather than a sync.WaitGroup, and that is a correctness
// requirement, not taste: handleStatusChanged runs on the bus's delivery
// goroutine while Wait runs on whoever is shutting down, so a WaitGroup's rule
// that an Add off zero happens-before Wait cannot hold here, and the counter
// can be read empty with a push still on its way in — a drain that ends before
// the last push, leaving a rider's lock screen frozen on the previous state.
// The package comment on internal/drain carries the full argument. MYR-398 is
// where it was first paid for, on this notifier; MYR-410 is where the alert
// notifier and the nav dispatcher turned out to carry the identical bug, and
// the counting moved into one place rather than a third copy.

// Wait blocks until every in-flight update finishes.
//
// Like the alert notifier's, this is HALF of the shutdown guarantee: it covers
// updates that have STARTED, and an event still sitting in this subscriber's
// buffered channel has not reached a handler at all. bus.Close is what makes
// that backlog started work; only the pair means "everything accepted was
// delivered" (MYR-410).
func (a *ActivityNotifier) Wait() { a.workers.Wait() }

// WaitContext is Wait with a deadline, returning how many updates were still in
// flight when it gave up (0 on a clean drain). Shutdown uses this — see
// Notifier.WaitContext for why an unbounded wait is unsafe there.
func (a *ActivityNotifier) WaitContext(ctx context.Context) (int, error) {
	inFlight, err := a.workers.WaitContext(ctx)
	if err != nil {
		return inFlight, fmt.Errorf("push.ActivityNotifier.WaitContext (%d update(s) abandoned): %w", inFlight, err)
	}
	return 0, nil
}

// track registers work that runs on the CALLER's goroutine with the same drain
// async's workers use, and returns the function that retires it.
//
// The ticker's held-end pass (MYR-421) is why this exists. It reaches send —
// and the progress and phase-mark writes behind it — synchronously on the
// ticker goroutine, without ever passing through async, so until it was counted
// here Wait() returned at shutdown while those pushes and writes were still in
// flight. That is the same silently-empty-counter failure internal/drain
// describes, arriving by a different door: not a WaitGroup racing an Add, but a
// whole code path the counter never knew about.
//
//	done := a.track()
//	defer done()
//
// No deadlock against Wait: Cond.Wait releases the drain's mutex while parked,
// so an increment can land while a drain is in progress — and one that lands
// after Wait has returned belongs to the next drain, exactly as async's does.
func (a *ActivityNotifier) track() func() { return a.workers.Track() }

// async runs fn on a bounded worker with its own timeout, detached from the
// bus so a slow APNs round-trip never backs up event delivery.
//
// The work is counted HERE, on the caller's goroutine, and not inside the
// worker: counting after the go statement would reopen the very window the
// drain exists to close.
func (a *ActivityNotifier) async(fn func(context.Context)) {
	done := a.workers.Track()

	go func() {
		defer done()
		a.sem <- struct{}{}
		defer func() { <-a.sem }()

		ctx, cancel := context.WithTimeout(context.Background(), a.cfg.Timeout)
		defer cancel()
		fn(ctx)
	}()
}
