// One sweep pass and its bounded worker pool (MYR-179). Split from
// reservation_sweeper.go so the type/config/loop stay separate from the pass,
// and from reservation_worker.go so the pass stays separate from the
// per-reservation policy.

package dispatch

import (
	"context"
	"log/slog"
	"sync"
)

// sweepDecision is what one worker did with one candidate. Returned rather
// than counted in place so the per-reservation policy (reservation_worker.go)
// stays free of shared mutable state.
type sweepDecision int

const (
	// decisionHeld: nothing was claimed and nothing was pushed — the vehicle
	// is busy inside the lateness window, the busy state was unreadable, the
	// claim errored, or shutdown began. The row is untouched and the next tick
	// re-decides.
	decisionHeld sweepDecision = iota
	// decisionDispatched: the claim was won and the push pipeline ran.
	decisionDispatched
	// decisionExpired: past the lateness ceiling — claimed and failed
	// honestly, with no push.
	decisionExpired
	// decisionLost: the claim went to a peer sweeper, or the ride was
	// cancelled/advanced between the SELECT and the claim. Silent by design.
	decisionLost
	// decisionWaiting: the candidate is inside the SELECT horizon but before
	// its computed leave-time (MYR-535). Untouched, unlogged per row, and
	// re-decided next tick with the car's newer position.
	decisionWaiting
)

// sweepResult counts one pass, for logging and tests.
type sweepResult struct {
	due        int // reservations selected as candidates
	dispatched int // claims won and pushed
	held       int // skipped this tick: vehicle busy, or unknown state
	expired    int // failed honestly: past the lateness ceiling
	lost       int // claim lost to a peer sweeper, or the ride moved on
	waiting    int // inside the horizon, before the computed leave-time
	// lapsed counts the SECOND pass (MYR-555): rows that were dispatched and
	// never driven, resolved at the same ceiling. Counted separately from
	// `expired` because the two arms resolve different populations through
	// different statements, and an operator watching one climb wants to know
	// which.
	lapsed int
}

func (r *sweepResult) count(d sweepDecision) {
	switch d {
	case decisionDispatched:
		r.dispatched++
	case decisionExpired:
		r.expired++
	case decisionLost:
		r.lost++
	case decisionHeld:
		r.held++
	case decisionWaiting:
		r.waiting++
	}
}

// sweepOnce runs one pass: SELECT what is due and actionable, then hand each
// candidate to a worker. The pass itself claims NOTHING — every latch is
// stamped inside a worker, after it holds a slot and immediately before it
// pushes (see handleDue). That is the whole point of the split: a candidate
// this pass never reaches is simply re-selected next tick, whereas a row
// claimed up-front and then abandoned by a deploy is burned forever.
//
// A list error ends the pass; the next tick retries, so no transient database
// failure can strand a reservation while it is still inside its window.
func (s *ReservationSweeper) sweepOnce(ctx context.Context) sweepResult {
	now := s.now()

	// SweepTimeout bounds the LIST only. The per-reservation work that follows
	// gets its own OverallTimeout-bounded context, exactly like an instant
	// dispatch — a 30s pass budget must never guillotine a nav push mid-flight
	// and leave the row claimed-but-unresolved.
	// The horizon is now + LeadCap (MYR-535): scheduledFor is an ARRIVAL
	// instant, so a candidate must be visible for the whole window it could
	// legally leave in. The per-row leave-time gate lives in the worker
	// (holdIfBeforeLeaveTime); the lateness deadline stays anchored on the
	// bare clock.
	listCtx, cancel := context.WithTimeout(ctx, s.cfg.SweepTimeout)
	dueList, err := s.store.ListDueReservations(listCtx, now.Add(s.cfg.LeadCap), now.Add(-s.cfg.MaxLateness), s.cfg.MaxPerSweep)
	cancel()
	if err != nil {
		s.logger.Error("reservation sweep: list due failed", slog.String("error", err.Error()))
		return sweepResult{}
	}

	res := sweepResult{due: len(dueList)}
	var (
		mu sync.Mutex
		wg sync.WaitGroup
	)
	// Indexed rather than ranged by value: DueReservation is wide enough that
	// gocritic flags the per-iteration copy.
	for i := range dueList {
		r := &dueList[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			d := s.runWorker(ctx, r)
			mu.Lock()
			defer mu.Unlock()
			res.count(d)
		}()
	}
	wg.Wait()

	// MYR-555's second pass: reservations the FIRST pass can never see, because
	// its `dispatched_at IS NULL` conjunct drops a row the moment it is claimed.
	// Runs after the dispatch phase, not before it — a live dispatch must never
	// wait behind a backlog of rows whose ride is already hours gone.
	res.lapsed = s.sweepLapsed(ctx, now)

	if res.due > 0 || res.lapsed > 0 {
		s.logger.Info("reservation sweep",
			slog.Int("due", res.due),
			slog.Int("dispatched", res.dispatched),
			slog.Int("held", res.held),
			slog.Int("expired", res.expired),
			slog.Int("lost", res.lost),
			slog.Int("waiting", res.waiting),
			slog.Int("lapsed", res.lapsed),
		)
	}
	return res
}

// runWorker acquires one of the sweeper's own slots and then runs the
// per-reservation decision under a fresh, OverallTimeout-bounded context.
//
// Two properties are load-bearing:
//
//   - The slot is the SWEEPER's (cfg.MaxConcurrent), never the dispatcher's.
//     A backlog of asleep-vehicle reservation pushes therefore cannot queue in
//     front of an instant accept's pickup push; instant dispatch behaviour is
//     unchanged by MYR-179 under any reservation load.
//   - The work context is DETACHED from the sweep context (values kept,
//     cancellation dropped) and re-bounded by OverallTimeout — the same
//     contract dispatchAsync gives an instant dispatch. Once we decide to
//     claim, the claim→outcome window is bounded and comfortably under the
//     startup reconciler's 5-minute age floor, so an interrupted claim is
//     always recoverable.
//
// Shutdown is honoured at the only point where it is free: BEFORE the claim.
// A cancelled context means a slot may never come, so we give the candidate
// back to the next process rather than latching it on the way out.
func (s *ReservationSweeper) runWorker(ctx context.Context, r *DueReservation) sweepDecision {
	select {
	case s.sem <- struct{}{}:
	case <-ctx.Done():
		return decisionHeld
	}
	defer func() { <-s.sem }()

	if ctx.Err() != nil {
		return decisionHeld
	}

	workCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.dispatcher.cfg.OverallTimeout)
	defer cancel()
	return s.handleDue(workCtx, r)
}
