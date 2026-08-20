// The two DB-only passes of the sweep — the halves that need no Tesla call and
// no token, and so complete even when every owner is gone and the Fleet API is
// unreachable. Split out of sweep.go for the 300-line file cap; Run still owns
// the order they happen in, and the order is argued at each call site.

package fleetorphan

import (
	"context"
	"log/slog"
)

// sweepAttempts handles source C: go_fleet_config_attempts rows whose vehicle
// no longer exists. No VIN, so nothing to ask Tesla — pure DB garbage.
func (s *Sweeper) sweepAttempts(ctx context.Context, apply bool, rep *Report) {
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	rows, err := s.deps.Store.ListOrphanFleetConfigAttempts(callCtx)
	cancel()
	if err != nil {
		rep.Errors = append(rep.Errors, "list orphan fleet-config attempts: "+errText(err))
		return
	}
	rep.Attempts.OrphanRows = rows
	if len(rows) == 0 || !apply {
		return
	}

	delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.CallTimeout)
	defer cancel()
	n, err := s.deps.Store.DeleteFleetConfigAttempts(delCtx, rows)
	if err != nil {
		rep.Errors = append(rep.Errors, "delete orphan fleet-config attempts: "+errText(err))
		return
	}
	rep.Attempts.Deleted = n
}

// purgeOrphanTombstones handles the MYR-596 legacy backlog: go_removed_vehicles
// rows whose user_id resolves to no identity source at all, left behind by
// accounts deleted before step 8e of the deletion sequence started removing
// them. The predicate lives in the store; see orphan_tombstone_purge.go for why
// a converged person's abandoned cuid is deliberately not matched by it.
//
// Counted first and always, deleted only under apply, so a dry run answers "how
// big is the backlog" without touching it. A failure lands in rep.Errors like
// every other partial result — the VIN work above is worth reporting whatever
// happens here.
func (s *Sweeper) purgeOrphanTombstones(ctx context.Context, apply bool, rep *Report) {
	if !s.cfg.PurgeOrphanTombstones {
		return
	}
	rep.Tombstones.Requested = true

	countCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	n, err := s.deps.Store.CountOrphanedTombstones(countCtx)
	cancel()
	if err != nil {
		rep.Errors = append(rep.Errors, "count orphaned removed-vehicle tombstones: "+errText(err))
		return
	}
	rep.Tombstones.Orphaned = n
	if n == 0 || !apply {
		return
	}

	// Detached from ctx for the same reason the config delete is: a write that
	// has started finishes or fails on its own terms, not on a Ctrl-C.
	delCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.CallTimeout)
	defer cancel()
	deleted, err := s.deps.Store.PurgeOrphanedTombstones(delCtx)
	if err != nil {
		rep.Errors = append(rep.Errors, "purge orphaned removed-vehicle tombstones: "+errText(err))
		return
	}
	rep.Tombstones.Deleted = deleted
	s.logger.Info("orphaned removed-vehicle tombstones purged",
		slog.Int64("deleted", deleted))
}
