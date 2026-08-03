package store

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The drive retention pruner (MYR-439, NFR-3.27, docs/contracts/
// data-lifecycle.md §5).
//
// Drive history and the full GPS trail on it are the most sensitive thing this
// service keeps, and the design docs have promised a one-year ceiling on them
// since Phase 1 with no code behind the promise. This is the enforcement.
//
// IT WILL DELETE NOTHING FOR MONTHS, AND THAT IS THE EXPECTED STATE. As of
// 2026-08-02 production holds 315 Drive rows, the oldest created 2026-03-08 —
// the platform is younger than its own retention window, so the first row
// becomes eligible on 2027-03-08. Shipping now is about having the enforcement
// in place and observed before the window first bites, not about clearing a
// backlog; there is no backlog. The practical consequence for anyone reading
// this in March 2027: until that date every production pass claims zero rows,
// so the audit grouping, the SKIP LOCKED contention path and Postgres TOAST
// reclamation for the deleted trails have been exercised ONLY by the tests in
// drive_pruner_test.go and never against real data. Treat the first pass that
// actually deletes something as the real first run, and watch it.
//
// WHY THE WHOLE ROW AND NOTHING ELSE. A drive's route/GPS trail is not a child
// record — it is two columns ON the drive row ("routePoints" JSONB plaintext
// and "routePointsEnc" ciphertext, MYR-64). Nothing anywhere is keyed by a
// drive id: there is no drive-keyed sidecar, no derivative table, and no FK
// pointing at "Drive" in either the Prisma schema or the go_* namespace (a Go
// migration could not declare one — CG-DL-9). So `DELETE FROM "Drive"` is the
// complete deletion, and deliberately NAMES NO COLUMN. That is what makes this
// pruner immune to MYR-433: when the plaintext "routePoints" column is retired,
// not one statement here changes, because none of them mention it. A
// column-scrubbing UPDATE would have been the thing that broke.
//
// See DrivePruner.PruneBatch for the per-batch transaction shape.

// DriveRetentionDays is THE retention window, in days, and the only place it is
// written. Every statement below derives its boundary from this constant.
//
// It is a compile-time constant and NOT configurable at runtime, on purpose:
// contract-guard CG-DL-4 requires it, because a retention window that an
// environment variable can shorten is one typo away from deleting a fleet's
// entire history. Changing it means changing docs/contracts/data-lifecycle.md
// §2.2 first, then this line.
const DriveRetentionDays = 365

// DriveRetentionWindow is DriveRetentionDays as a duration, for callers and
// tests that need to reason about the boundary in Go rather than in SQL. It is
// derived from the constant above, never written independently.
const DriveRetentionWindow = DriveRetentionDays * 24 * time.Hour

// DefaultPruneBatchSize is the number of drives one prune transaction deletes
// (data-lifecycle.md §5.5). Large enough that a year of backlog clears in a
// sane number of round trips, small enough that the row locks a single
// transaction holds on the Prisma-owned "Drive" table are never interesting to
// a concurrent reader.
const DefaultPruneBatchSize = 100

// queryPrunableDriveBatch claims one batch of expired drives.
//
// Three things about this statement are load-bearing:
//
//   - It JOINs "Vehicle" only to resolve the drive's owner, which the audit row
//     needs (§5.4 groups the audit entry by vehicle owner). "Vehicle"."userId"
//     is NOT NULL and "Drive"."vehicleId" is an FK, so the join never drops a
//     row that should be pruned.
//   - ORDER BY "createdAt" ASC makes the sweep oldest-first, so a backlog drains
//     in the order the data aged and a budget-capped pass always makes progress
//     against the worst offenders.
//   - FOR UPDATE OF d SKIP LOCKED is what replaces §5.8's "leader-elected"
//     requirement with something that needs no coordinator. Two replicas that
//     both wake at 03:00 UTC claim DISJOINT batches instead of colliding on the
//     same rows; a replica that dies mid-batch releases its claim on rollback.
//     SKIP LOCKED is correct here precisely because the work set is unordered
//     with respect to correctness — any expired drive may be deleted by anyone.
//
// It selects no P1 column: not "routePoints", not the location strings. The
// pruner never reads the data it destroys.
var queryPrunableDriveBatch = fmt.Sprintf(`
SELECT d."id", d."vehicleId", v."userId", d."createdAt"
FROM "Drive" d
JOIN "Vehicle" v ON v."id" = d."vehicleId"
WHERE d."createdAt" < NOW() - make_interval(days => %d)
ORDER BY d."createdAt" ASC
LIMIT $1
FOR UPDATE OF d SKIP LOCKED`, DriveRetentionDays)

// queryPruneDeleteDrives deletes one claimed batch by id.
//
// The age predicate is REPEATED here rather than trusted from the SELECT. The
// ids are already claimed and locked, so this cannot change the outcome in
// normal operation — it exists so that no future edit to the claim query can
// turn this statement into an unbounded delete. CG-DL-4's boundary is asserted
// at the point of destruction, not merely upstream of it.
//
// It names no column, so the row goes whole: "routePoints" (plaintext trail),
// "routePointsEnc" (ciphertext trail), and the four plaintext location/address
// strings all die with it. This is the statement MYR-433 does not touch.
var queryPruneDeleteDrives = fmt.Sprintf(`
DELETE FROM "Drive"
WHERE "id" = ANY($1)
  AND "createdAt" < NOW() - make_interval(days => %d)`, DriveRetentionDays)

// prunedDrive is one claimed row, carried from the SELECT to the audit grouping.
type prunedDrive struct {
	id        string
	vehicleID string
	userID    string
	createdAt time.Time
}

// PruneBatchResult is the log-safe (P0) outcome of one PruneBatch call.
type PruneBatchResult struct {
	// DrivesDeleted is how many "Drive" rows this batch destroyed.
	DrivesDeleted int
	// AuditRows is how many drives_pruned entries were written — one per
	// (owner, vehicle) group in the batch, per §5.5.
	AuditRows int
	// Exhausted is true ONLY when the claim returned zero rows. The pass loop's
	// terminator.
	//
	// NOT "the batch came back short". Under SKIP LOCKED a short batch is
	// ambiguous: it means "no more rows I can claim", which is either "none are
	// left" OR "a peer holds the rest". Treating the second as success lets this
	// replica declare the backlog drained over rows that a dying peer is about
	// to roll back — the sweep would report completion and then the gauge would
	// advance on work nobody did. Requiring an empty claim costs one extra
	// round trip per pass and removes the ambiguity entirely.
	Exhausted bool
}

// prunerAuditMetadata is the P0-only metadata on a drives_pruned row (CG-DL-5):
// a count and two timestamps of already-destroyed rows. No coordinates, no
// addresses, no drive ids.
type prunerAuditMetadata struct {
	DriveCount      int    `json:"driveCount"`
	OldestDriveDate string `json:"oldestDriveDate"`
	NewestDriveDate string `json:"newestDriveDate"`
}

// DrivePruner deletes drives past the retention window, one bounded batch per
// call. It is the store-side half of the retention sweep; the cadence, the
// per-pass budget and the retries live in internal/retention.
type DrivePruner struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewDrivePruner builds the pruner over the given pool.
func NewDrivePruner(pool *pgxpool.Pool, logger *slog.Logger) *DrivePruner {
	if logger == nil {
		logger = slog.Default()
	}
	return &DrivePruner{pool: pool, logger: logger}
}

// PruneBatch claims, audits and deletes up to batchSize expired drives in ONE
// transaction, and reports what it destroyed.
//
// Sequence (§5.4, CG-DL-3):
//
//  1. Claim the oldest expired drives FOR UPDATE SKIP LOCKED.
//  2. Nothing claimed → commit the empty transaction, report Exhausted.
//  3. INSERT one drives_pruned audit row per (owner, vehicle) group — BEFORE
//     the delete, so a failure to record the deletion prevents the deletion
//     rather than hiding it.
//  4. DELETE the claimed rows.
//
// Atomic and idempotent: on any error nothing is deleted and no audit row
// survives, and a re-run simply re-claims whatever is still expired. Because
// the whole batch is one transaction, a cancelled context aborts cleanly at a
// batch boundary — there is no half-pruned state to resume from.
func (p *DrivePruner) PruneBatch(ctx context.Context, batchSize int) (PruneBatchResult, error) {
	if batchSize <= 0 {
		return PruneBatchResult{}, fmt.Errorf("store.PruneBatch: non-positive batch size %d", batchSize)
	}

	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return PruneBatchResult{}, fmt.Errorf("store.PruneBatch: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after Commit

	claimed, err := claimExpiredDrives(ctx, tx, batchSize)
	if err != nil {
		return PruneBatchResult{}, err
	}
	if len(claimed) == 0 {
		if err := tx.Commit(ctx); err != nil {
			return PruneBatchResult{}, fmt.Errorf("store.PruneBatch: commit empty: %w", err)
		}
		return PruneBatchResult{Exhausted: true}, nil
	}

	// (3) Audit FIRST (CG-DL-3).
	auditRows, err := insertPruneAudits(ctx, tx, claimed)
	if err != nil {
		return PruneBatchResult{}, err
	}

	// (4) Destroy the rows — whole, columns unnamed.
	ids := make([]string, 0, len(claimed))
	for i := range claimed {
		ids = append(ids, claimed[i].id)
	}
	tag, err := tx.Exec(ctx, queryPruneDeleteDrives, ids)
	if err != nil {
		return PruneBatchResult{}, fmt.Errorf("store.PruneBatch: delete drives: %w", err)
	}
	deleted := int(tag.RowsAffected())

	if err := tx.Commit(ctx); err != nil {
		return PruneBatchResult{}, fmt.Errorf("store.PruneBatch: commit: %w", err)
	}

	// P0 only: counts and an opaque window. Never a drive id or a coordinate.
	p.logger.Info("drives_pruned",
		slog.String("event", "drives_pruned"),
		slog.Int("drives_deleted", deleted),
		slog.Int("audit_rows", auditRows),
		slog.Int("retention_days", DriveRetentionDays),
	)

	return PruneBatchResult{
		DrivesDeleted: deleted,
		AuditRows:     auditRows,
		// Always false here: this call claimed rows, so the only honest report
		// of "nothing left" comes from a subsequent claim returning zero. See
		// the field's doc comment for why a short batch does not qualify.
		Exhausted: false,
	}, nil
}

// claimExpiredDrives runs the claim query inside the batch transaction.
func claimExpiredDrives(ctx context.Context, tx pgx.Tx, batchSize int) ([]prunedDrive, error) {
	rows, err := tx.Query(ctx, queryPrunableDriveBatch, batchSize)
	if err != nil {
		return nil, fmt.Errorf("store.PruneBatch: claim batch: %w", err)
	}
	defer rows.Close()

	claimed := make([]prunedDrive, 0, batchSize)
	for rows.Next() {
		var d prunedDrive
		if err := rows.Scan(&d.id, &d.vehicleID, &d.userID, &d.createdAt); err != nil {
			return nil, fmt.Errorf("store.PruneBatch: scan claimed drive: %w", err)
		}
		claimed = append(claimed, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.PruneBatch: iterate claimed drives: %w", err)
	}
	return claimed, nil
}

// insertPruneAudits writes one drives_pruned row per (owner, vehicle) group in
// the batch, per §5.5's "one audit entry per batch per vehicle owner". Grouping
// by vehicle rather than emitting one row per drive keeps the audit history
// readable at 100 drives a batch, and grouping by vehicle rather than by owner
// alone is what lets targetId be the vehicle id the contract specifies.
//
// It reuses the same-package queryAuditInsert column list, so CG-DL-8 column
// parity with the Prisma model stays automatic (same reasoning as
// OwnerTeardown.insertAudit).
func insertPruneAudits(ctx context.Context, tx pgx.Tx, claimed []prunedDrive) (int, error) {
	type group struct {
		userID string
		count  int
		oldest time.Time
		newest time.Time
	}
	// Insertion-ordered so the audit rows land oldest-vehicle-first and the
	// output is deterministic for tests.
	order := make([]string, 0, len(claimed))
	groups := make(map[string]*group, len(claimed))
	for i := range claimed {
		d := &claimed[i]
		g, ok := groups[d.vehicleID]
		if !ok {
			g = &group{userID: d.userID, oldest: d.createdAt, newest: d.createdAt}
			groups[d.vehicleID] = g
			order = append(order, d.vehicleID)
		}
		g.count++
		if d.createdAt.Before(g.oldest) {
			g.oldest = d.createdAt
		}
		if d.createdAt.After(g.newest) {
			g.newest = d.createdAt
		}
	}

	now := time.Now().UTC()
	for _, vehicleID := range order {
		g := groups[vehicleID]
		meta, err := json.Marshal(prunerAuditMetadata{
			DriveCount:      g.count,
			OldestDriveDate: g.oldest.UTC().Format(time.RFC3339),
			NewestDriveDate: g.newest.UTC().Format(time.RFC3339),
		})
		if err != nil {
			return 0, fmt.Errorf("store.PruneBatch: marshal audit metadata: %w", err)
		}
		if _, err := tx.Exec(ctx, queryAuditInsert,
			newProvisionID(),                // id (cuid)
			g.userID,                        // userId (owner of the pruned data)
			now,                             // timestamp
			string(AuditActionDrivesPruned), // action
			auditTargetTypeDrive,            // targetType
			vehicleID,                       // targetId (the owner's car)
			auditInitiatorSystemPruner,      // initiator
			meta,                            // metadata (P0 counts + window)
			now,                             // createdAt
		); err != nil {
			return 0, fmt.Errorf("store.PruneBatch: insert audit: %w", err)
		}
	}
	return len(order), nil
}
