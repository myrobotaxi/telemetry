package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The ride retention pruner (MYR-447, docs/contracts/data-lifecycle.md §5).
//
// go_ride_requests keeps terminal rides forever today. Each one holds two
// AES-256-GCM coordinate pairs, two plaintext place labels, two plaintext street
// addresses and — on legacy rows — a booked-for passenger's name and phone. It
// is the second-most sensitive table this service owns after "Drive", and unlike
// "Drive" it had no ceiling at all. This is the ceiling.
//
// TWO JOBS, ONE TRANSACTION PER BATCH:
//
//  1. DELETE terminal rides past RideRetentionDays.
//  2. SCRUB passenger_name + passenger_phone on terminal rides past
//     RidePassengerScrubDays that still carry them.
//
// The delete runs FIRST, and that ordering is not incidental: the scrub's target
// set is a superset of the delete's by age, so scrubbing first would rewrite
// rows that are about to be destroyed — a wasted UPDATE and a misleading
// ride_passengers_scrubbed audit row about data that no longer exists.
//
// TERMINAL MEANS EXACTLY completed / declined / cancelled. See
// rideTerminalStatusPredicate in ride_pruner_queries.go for the list and for why
// a `reservation_expired` ride — which is still `accepted`, i.e. LIVE — is not
// in it.
//
// See RidePruner.PruneBatch for the per-batch transaction shape.

// RideRetentionDays is THE ride retention window, in days, and the only place it
// is written. Every statement in ride_pruner_queries.go derives its boundary
// from this constant.
//
// It is a compile-time constant and NOT configurable at runtime, on purpose:
// contract-guard CG-DL-4 requires it, because a retention window that an
// environment variable can shorten is one typo away from deleting a fleet's
// entire ride history. Changing it means changing
// docs/contracts/data-lifecycle.md §2 first, then this line.
const RideRetentionDays = 365

// RideRetentionWindow is RideRetentionDays as a duration, for callers and tests
// that need to reason about the boundary in Go rather than in SQL. It is derived
// from the constant above, never written independently.
const RideRetentionWindow = RideRetentionDays * 24 * time.Hour

// RidePassengerScrubDays is how long after a ride goes terminal its deprecated
// booked-for-passenger columns are NULLed. Thirty days, not 365, because unlike
// the rest of the row this data has NO remaining purpose: the
// book-for-someone-else feature was removed from the app in MYR-382, no live
// surface renders a terminal ride's passenger, and nothing but the REST
// projection reads the columns at all. Data with no reader should not wait out
// the full record retention window; 30 days is a support-window's grace on a
// feature that is already gone.
//
// Same CG-DL-4 posture as RideRetentionDays: compile-time, unreachable from
// config.
const RidePassengerScrubDays = 30

// RidePassengerScrubWindow is RidePassengerScrubDays as a duration, for callers
// and tests reasoning about the boundary in Go.
const RidePassengerScrubWindow = RidePassengerScrubDays * 24 * time.Hour

// DefaultRidePruneBatchSize is the number of rides one prune transaction
// handles, for each of its two jobs. Small enough that the row locks a single
// transaction holds — on go_ride_requests AND, via the migration 0025 cascade,
// on go_live_activities — are never interesting to a concurrent reader; large
// enough that a sizeable backlog would clear in a sane number of round trips.
const DefaultRidePruneBatchSize = 100

// prunedRide is one claimed row, carried from a claim query to the audit
// grouping. It holds no P1 field, by construction — see the claim queries.
type prunedRide struct {
	id        string
	vehicleID string
	ownerID   string
	updatedAt time.Time
}

// RidePruneBatchResult is the log-safe (P0) outcome of one PruneBatch call.
type RidePruneBatchResult struct {
	// RidesDeleted is how many go_ride_requests rows this batch destroyed.
	RidesDeleted int
	// PassengersScrubbed is how many SURVIVING rows had both passenger columns
	// NULLed. Disjoint from RidesDeleted: the delete commits first within the
	// transaction, so the scrub claim cannot see a deleted row.
	PassengersScrubbed int
	// AuditRows is how many audit entries were written across both jobs — one
	// per (owner, vehicle) group per job.
	AuditRows int
	// Exhausted is true ONLY when BOTH claims returned zero rows. The pass
	// loop's terminator.
	//
	// NOT "the batches came back short". Under SKIP LOCKED a short batch is
	// ambiguous: it means "no more rows I can claim", which is either "none are
	// left" OR "a peer holds the rest". Treating the second as success lets this
	// replica declare the backlog drained over rows that a dying peer is about
	// to roll back — the sweep would report completion and then the gauge would
	// advance on work nobody did. Requiring empty claims costs one extra round
	// trip per pass and NARROWS that ambiguity without removing it: if a peer
	// holds the ENTIRE remainder, both claims come back empty here too, and
	// this replica still reports a complete pass over rows it never saw. The
	// residual is bounded and worth accepting — the pass is idempotent, so the
	// next night re-claims anything a peer rolled back, and closing it properly
	// would need cross-replica coordination that costs more than a one-day
	// delay on a 365-day window.
	Exhausted bool
}

// RidePruner enforces the ride retention window, one bounded batch per call. It
// is the store-side half of the ride retention sweep; the cadence, the per-pass
// budget and the retries live in internal/retention.
type RidePruner struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewRidePruner builds the pruner over the given pool.
func NewRidePruner(pool *pgxpool.Pool, logger *slog.Logger) *RidePruner {
	if logger == nil {
		logger = slog.Default()
	}
	return &RidePruner{pool: pool, logger: logger}
}

// PruneBatch claims, audits, deletes and scrubs up to batchSize rides each in
// ONE transaction, and reports what it did.
//
// Sequence (§5.4, CG-DL-3):
//
//  1. Claim the oldest expired TERMINAL rides FOR UPDATE SKIP LOCKED.
//  2. If any: INSERT one rides_pruned audit row per (owner, vehicle) group —
//     BEFORE the delete, so a failure to record the deletion prevents the
//     deletion rather than hiding it — then DELETE the claimed rows.
//  3. Claim the oldest terminal rides still carrying passenger data.
//  4. If any: INSERT one ride_passengers_scrubbed audit row per (owner, vehicle)
//     group, then NULL both passenger columns.
//  5. Report Exhausted only if BOTH claims were empty.
//
// Atomic and idempotent: on any error nothing is deleted or scrubbed and no
// audit row survives, and a re-run simply re-claims whatever is still eligible.
// Because the whole batch is one transaction, a cancelled context aborts cleanly
// at a batch boundary — there is no half-pruned state to resume from.
func (p *RidePruner) PruneBatch(ctx context.Context, batchSize int) (RidePruneBatchResult, error) {
	if batchSize <= 0 {
		return RidePruneBatchResult{}, fmt.Errorf("store.RidePruner.PruneBatch: non-positive batch size %d", batchSize)
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return RidePruneBatchResult{}, fmt.Errorf("store.RidePruner.PruneBatch: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	res := RidePruneBatchResult{}

	del, err := p.runDeleteJob(ctx, tx, batchSize)
	if err != nil {
		return RidePruneBatchResult{}, err
	}
	res.RidesDeleted = del.written
	res.AuditRows += del.audits

	scrub, err := p.runScrubJob(ctx, tx, batchSize)
	if err != nil {
		return RidePruneBatchResult{}, err
	}
	res.PassengersScrubbed = scrub.written
	res.AuditRows += scrub.audits

	if err := tx.Commit(ctx); err != nil {
		return RidePruneBatchResult{}, fmt.Errorf("store.RidePruner.PruneBatch: commit: %w", err)
	}

	// Exhausted only when NEITHER job could claim anything — see the field's
	// doc comment for why a short batch does not qualify.
	res.Exhausted = del.claimed == 0 && scrub.claimed == 0

	if res.RidesDeleted > 0 || res.PassengersScrubbed > 0 {
		// P0 only: counts and the two window sizes. Never a ride id, a
		// coordinate, an address or a passenger value.
		p.logger.Info("rides_pruned",
			slog.String("event", "rides_pruned"),
			slog.Int("rides_deleted", res.RidesDeleted),
			slog.Int("passengers_scrubbed", res.PassengersScrubbed),
			slog.Int("audit_rows", res.AuditRows),
			slog.Int("retention_days", RideRetentionDays),
			slog.Int("passenger_scrub_days", RidePassengerScrubDays),
		)
	}

	return res, nil
}

// rideJobResult is one of the two jobs' contribution to a batch.
//
// `claimed` and `written` are separate on purpose. Exhausted is derived from
// CLAIMED, never from written: a claim that found rows but whose write affected
// none — the repeated guards rejecting a row that changed under the lock — is
// still evidence that work may remain, and reporting exhaustion there would end
// the pass over rows nobody handled.
type rideJobResult struct {
	// claimed is how many rows the claim query locked.
	claimed int
	// written is how many rows the destructive statement actually affected.
	written int
	// audits is how many audit entries were written for this job.
	audits int
}

// runDeleteJob is step 1-2: claim, audit, destroy.
func (p *RidePruner) runDeleteJob(ctx context.Context, tx pgx.Tx, batchSize int) (rideJobResult, error) {
	claimed, err := claimRides(ctx, tx, queryPrunableRideBatch, batchSize, "claim expired rides")
	if err != nil {
		return rideJobResult{}, err
	}
	if len(claimed) == 0 {
		return rideJobResult{}, nil
	}

	// Audit FIRST (CG-DL-3).
	audits, err := insertRideAudits(ctx, tx, claimed, AuditActionRidesPruned)
	if err != nil {
		return rideJobResult{}, err
	}

	tag, err := tx.Exec(ctx, queryPruneDeleteRides, rideIDs(claimed))
	if err != nil {
		return rideJobResult{}, fmt.Errorf("store.RidePruner.PruneBatch: delete rides: %w", err)
	}
	return rideJobResult{claimed: len(claimed), written: int(tag.RowsAffected()), audits: audits}, nil
}

// runScrubJob is step 3-4: claim, audit, NULL both passenger columns.
func (p *RidePruner) runScrubJob(ctx context.Context, tx pgx.Tx, batchSize int) (rideJobResult, error) {
	claimed, err := claimRides(ctx, tx, queryScrubbableRideBatch, batchSize, "claim scrubbable rides")
	if err != nil {
		return rideJobResult{}, err
	}
	if len(claimed) == 0 {
		return rideJobResult{}, nil
	}

	audits, err := insertRideAudits(ctx, tx, claimed, AuditActionRidePassengersScrubbed)
	if err != nil {
		return rideJobResult{}, err
	}

	// ALWAYS BOTH COLUMNS, NEVER PHONE ONLY — the statement sets them together
	// and must keep doing so. A name-without-phone row renders as a leading
	// blank in the iOS passenger block (ScheduledRideSheet.swift:541); only
	// removing both makes the block disappear. See queryScrubRidePassengers.
	tag, err := tx.Exec(ctx, queryScrubRidePassengers, rideIDs(claimed))
	if err != nil {
		return rideJobResult{}, fmt.Errorf("store.RidePruner.PruneBatch: scrub passengers: %w", err)
	}
	return rideJobResult{claimed: len(claimed), written: int(tag.RowsAffected()), audits: audits}, nil
}

// rideIDs projects claimed rows onto the id array both write statements take.
func rideIDs(claimed []prunedRide) []string {
	ids := make([]string, 0, len(claimed))
	for i := range claimed {
		ids = append(ids, claimed[i].id)
	}
	return ids
}

// claimRides runs one of the two claim queries inside the batch transaction.
// Both project the same four columns, so one scanner serves both.
func claimRides(ctx context.Context, tx pgx.Tx, query string, batchSize int, what string) ([]prunedRide, error) {
	rows, err := tx.Query(ctx, query, batchSize)
	if err != nil {
		return nil, fmt.Errorf("store.RidePruner.PruneBatch: %s: %w", what, err)
	}
	defer rows.Close()

	claimed := make([]prunedRide, 0, batchSize)
	for rows.Next() {
		var r prunedRide
		if err := rows.Scan(&r.id, &r.vehicleID, &r.ownerID, &r.updatedAt); err != nil {
			return nil, fmt.Errorf("store.RidePruner.PruneBatch: scan %s: %w", what, err)
		}
		claimed = append(claimed, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.RidePruner.PruneBatch: iterate %s: %w", what, err)
	}
	return claimed, nil
}
