// Package nameconfirmbackfill is MYR-583's deploy-time data step, in two parts
// that run in one transaction:
//
//  1. SCRUB the legacy web onboarding's literal 'Tesla User' placeholder out of
//     `"User"."name"`, so it stops resolving as a name on any ladder.
//  2. BACKFILL `go_profile_name_confirmations` for the accounts that used the
//     rename prompt in the window between the endpoint shipping and this table
//     existing.
//
// # Why this is a Go binary and not a migration
//
// Both statements name the sibling app's Prisma-owned `"User"` table, and
// contract-guard rule CG-DL-9 forbids any file under internal/store/migrations/
// from so much as mentioning it (docs/contracts/data-lifecycle.md §7). The Go
// migration toolchain is scoped to the `_telemetry_*` and `go_*` namespaces, and
// migration 0042 — which creates the table this package fills — obeys that by
// naming nothing else.
//
// An application-runtime statement against a Prisma-owned table is the SANCTIONED
// class (the §1.4 carve-outs: OwnerProvisioner's upsert, the plate and colour
// UPDATEs, the MYR-581 name write). This is one of those, narrowed further by
// being an OPERATOR binary rather than a request path: the running server never
// imports this package, which is also why it lives outside internal/store.
//
// # Why the scrub is not left to the confirmation gate
//
// MYR-583's gate already makes 'Tesla User' harmless on the two counterparty
// surfaces and at the offerability gate: nobody confirmed it, so it reads as
// absent. The scrub is still worth running, for the surfaces the gate does NOT
// cover — `RideRequest.requesterName` and the redeem screen's ladder are
// deliberately ungated (see internal/store/ride_request_queries.go), and on those
// this string still renders as a person's name. 'Tesla User' is a machine-written
// placeholder that names nobody; it is the literal source of MYR-532 item 4's
// "Tesla wants a ride" report, and no gate makes it true.
//
// # Idempotent, and the two orders are both safe
//
// Re-running scrubs nothing (the predicate no longer matches) and backfills
// nothing (ON CONFLICT DO NOTHING). Running the backfill FIRST would also be
// safe: a placeholder account's `"updatedAt"` predates the window by months. The
// scrub is sequenced first anyway, so the invariant "a placeholder is never
// credited a confirmation" holds without depending on that fact.
package nameconfirmbackfill

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LegacyWebPlaceholderName is the exact string the 2026-03 react-frontend auth
// seeded into `"User"."name"` for accounts it created. Matched on the TRIMMED
// value and nothing else: this is a literal, not a pattern, and a fuzzy match
// here would be a fuzzy match against real people's names.
const LegacyWebPlaceholderName = "Tesla User"

// ConfirmationWindowStart is the lower bound of the backfill heuristic: the
// deploy instant of `PATCH /api/users/me` (MYR-581, 2026-08-17 ~07:00Z).
//
// ── THE HEURISTIC, AND ITS BOUND ─────────────────────────────────────────────
//
// The endpoint bumps `"User"."updatedAt"` on every successful rename, and until
// migration 0042 there was nowhere else to record that a rename happened. So a
// non-empty name on a row touched since the deploy is evidence — not proof — that
// the person went through the prompt.
//
// WHAT IT COULD GET WRONG, stated rather than waved at: it credits an account
// whose name was written TODAY by something other than the prompt. The only other
// writers of that column are the Next.js app's NextAuth/Prisma onboarding and
// `store.OwnerProvisioner`'s INSERT — and the whole point of MYR-583 is that a
// name from those sources should NOT count. That set is empty in practice (the web
// onboarding is not in use and provisioning INSERTs a name only when it has one
// from Tesla's account, which it does not), and the failure is bounded to
// accounts whose rows moved inside a single day.
//
// WHY THE ERROR IS ACCEPTABLE IN THIS DIRECTION. A wrongly-credited account shows
// its unconfirmed name for one more prompt cycle; a wrongly-UNcredited account is
// asked to confirm a name it already confirmed today. Both are recoverable by the
// client's prompt, and the first affects a set that is empty in practice while the
// second would affect everyone who used the endpoint on its first day.
var ConfirmationWindowStart = time.Date(2026, 8, 17, 7, 0, 0, 0, time.UTC)

// queryPlaceholderIDs finds the accounts still carrying the placeholder, taking a
// row lock so the audit row and the scrub cannot straddle a concurrent write.
//
// It selects the ID ONLY. The name is known already — it is the literal in the
// predicate — so there is nothing to read back, and a scrub that never holds a
// name value cannot log one.
const queryPlaceholderIDs = `
SELECT "id" FROM "User" WHERE TRIM("name") = $1 ORDER BY "id" FOR UPDATE`

// queryScrubPlaceholder clears the placeholder for the ids already audited.
//
// `"name"` becomes NULL rather than ”: NULL is the column's own "no name" value,
// it is what a fresh Apple-native account has, and every ladder rung wraps this
// column in `NULLIF(TRIM(...), ”)` so the two are equivalent to every reader
// anyway. NULL is the honest one.
//
// `"updatedAt"` is bumped so the sibling app observes the row as changed, matching
// what the MYR-581 writer does. It cannot corrupt the backfill below: that
// statement requires a NON-NULL name, and this one has just removed it.
const queryScrubPlaceholder = `
UPDATE "User" SET "name" = NULL, "updatedAt" = NOW() WHERE "id" = ANY($1)`

// queryBackfillConfirmations credits the accounts that renamed through the prompt
// before there was a table to record it in.
//
// `confirmed_at` takes the row's OWN `"updatedAt"`, not NOW(): the timestamp then
// records when the person actually confirmed, as far as anything can know, rather
// than when an operator ran a binary. That is also what makes a backfilled row
// distinguishable from a live one to anybody reading the table later.
//
// ON CONFLICT DO NOTHING, never DO UPDATE: a row already present was written by
// the endpoint itself and carries a real timestamp. An inferred one must never
// overwrite an observed one.
const queryBackfillConfirmations = `
INSERT INTO go_profile_name_confirmations (user_id, confirmed_at)
SELECT u."id", u."updatedAt"
FROM "User" u
WHERE NULLIF(TRIM(u."name"), '') IS NOT NULL
  AND u."updatedAt" >= $1
ON CONFLICT (user_id) DO NOTHING`

// queryCountBackfillCandidates is the dry-run counterpart: the same predicate,
// minus the rows a confirmation already covers.
const queryCountBackfillCandidates = `
SELECT COUNT(*)
FROM "User" u
WHERE NULLIF(TRIM(u."name"), '') IS NOT NULL
  AND u."updatedAt" >= $1
  AND NOT EXISTS (
      SELECT 1 FROM go_profile_name_confirmations c WHERE c.user_id = u."id"
  )`

// Result reports what a run did (or, on a dry run, would do).
type Result struct {
	// DryRun echoes the mode, so a report cannot be misread as a real run.
	DryRun bool
	// PlaceholdersScrubbed is the number of `"User"` rows whose placeholder name
	// was cleared. Expected: 1 in production.
	PlaceholdersScrubbed int
	// ConfirmationsBackfilled is the number of confirmation rows inserted.
	ConfirmationsBackfilled int
}

// auditor writes the trail for the scrub, inside the run's transaction. The
// production conformer is *store.ProfileNamePlaceholderAuditor; the interface is
// here, at the consumer, per project convention.
type auditor interface {
	RecordPlaceholderScrub(ctx context.Context, tx pgx.Tx, userID, placeholder string) error
}

// Runner performs the two steps over one pool.
type Runner struct {
	pool    *pgxpool.Pool
	logger  *slog.Logger
	dryRun  bool
	auditor auditor
}

// New builds a runner. A nil logger falls back to slog.Default.
func New(pool *pgxpool.Pool, logger *slog.Logger, dryRun bool) *Runner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Runner{pool: pool, logger: logger, dryRun: dryRun}
}

// WithAuditor supplies the audit writer. A REAL run REFUSES to proceed without
// one (see Run): an operator write to the sibling app's `"User"` table that leaves
// no trail is exactly the thing §1.4 carve-outs are not allowed to be. A dry run
// needs none, because it changes nothing.
func (r *Runner) WithAuditor(a auditor) *Runner {
	r.auditor = a
	return r
}

// ErrNoAuditor is returned when a real run has no audit writer.
var ErrNoAuditor = errors.New("nameconfirmbackfill: a real run requires an auditor")

// Run executes both steps. On a dry run it only COUNTS, in a transaction it rolls
// back, so even the counting cannot leave a lock or a row behind.
//
// ONE TRANSACTION for both steps. They are independent statements, so this is not
// about correctness between them — it is about the run being a single reviewable
// event: either the placeholder is gone and the confirmations are in, or the
// database is untouched and the operator reads an error instead of guessing which
// half landed.
func (r *Runner) Run(ctx context.Context) (Result, error) {
	res := Result{DryRun: r.dryRun}
	if !r.dryRun && r.auditor == nil {
		return res, ErrNoAuditor
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("nameconfirmbackfill: begin: %w", err)
	}
	// Rolls back every path that has not committed — including, deliberately,
	// every dry run.
	defer func() { _ = tx.Rollback(ctx) }()

	scrubbed, err := r.scrub(ctx, tx)
	if err != nil {
		return res, err
	}
	res.PlaceholdersScrubbed = scrubbed

	backfilled, err := r.backfill(ctx, tx)
	if err != nil {
		return res, err
	}
	res.ConfirmationsBackfilled = backfilled

	if r.dryRun {
		r.logger.Info("name confirmation backfill: DRY RUN, rolling back",
			slog.Int("placeholders_scrubbed", res.PlaceholdersScrubbed),
			slog.Int("confirmations_backfilled", res.ConfirmationsBackfilled))
		return res, nil
	}
	if err := tx.Commit(ctx); err != nil {
		return res, fmt.Errorf("nameconfirmbackfill: commit: %w", err)
	}
	r.logger.Info("name confirmation backfill: committed",
		slog.Int("placeholders_scrubbed", res.PlaceholdersScrubbed),
		slog.Int("confirmations_backfilled", res.ConfirmationsBackfilled))
	return res, nil
}

// scrub clears the legacy placeholder, writing one audit row per account first.
// On a dry run it stops after counting the ids — the row locks the SELECT took are
// held only until the caller's rollback, which is immediate on that path.
func (r *Runner) scrub(ctx context.Context, tx pgx.Tx) (int, error) {
	rows, err := tx.Query(ctx, queryPlaceholderIDs, LegacyWebPlaceholderName)
	if err != nil {
		return 0, fmt.Errorf("nameconfirmbackfill: select placeholder ids: %w", err)
	}
	ids := make([]string, 0, 4)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("nameconfirmbackfill: scan placeholder id: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("nameconfirmbackfill: read placeholder ids: %w", err)
	}
	if len(ids) == 0 || r.dryRun {
		return len(ids), nil
	}

	// Audit BEFORE the write (CG-DL-3's ordering), on the same transaction.
	for _, id := range ids {
		if err := r.auditor.RecordPlaceholderScrub(ctx, tx, id, LegacyWebPlaceholderName); err != nil {
			return 0, fmt.Errorf("nameconfirmbackfill: audit placeholder scrub: %w", err)
		}
	}
	tag, err := tx.Exec(ctx, queryScrubPlaceholder, ids)
	if err != nil {
		return 0, fmt.Errorf("nameconfirmbackfill: scrub placeholder: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// backfill credits the pre-table confirmers, or counts them on a dry run.
func (r *Runner) backfill(ctx context.Context, tx pgx.Tx) (int, error) {
	if r.dryRun {
		var n int
		if err := tx.QueryRow(ctx, queryCountBackfillCandidates, ConfirmationWindowStart).Scan(&n); err != nil {
			return 0, fmt.Errorf("nameconfirmbackfill: count candidates: %w", err)
		}
		return n, nil
	}
	tag, err := tx.Exec(ctx, queryBackfillConfirmations, ConfirmationWindowStart)
	if err != nil {
		return 0, fmt.Errorf("nameconfirmbackfill: backfill confirmations: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
