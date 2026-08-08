// Package passengerscrub NULLs the two deprecated booked-for-passenger
// columns on go_ride_requests, everywhere, once (MYR-447):
//
//   - go_ride_requests.passenger_name  (P1 plaintext, log-redacted)
//   - go_ride_requests.passenger_phone (P1 plaintext, log-redacted)
//
// WHY A ONE-TIME BINARY AND NOT JUST THE SWEEPER. The MYR-447 sweeper scrubs
// these columns at terminal + 30 days, which covers everything going forward
// and eventually covers the legacy rows too — but only as each one ages out,
// and never at all for a ride that is still OPEN. The book-for-someone-else
// feature was REMOVED from the app in MYR-382 and no live surface renders a
// passenger, so there is no reason for a single one of these values to survive
// another day. This clears the whole column in one pass, today.
//
// It is therefore deliberately BROADER than the sweeper: no age window, no
// status filter. Every row with either column populated is scrubbed.
//
// CRITICAL INVARIANT — ALWAYS SCRUB BOTH COLUMNS TOGETHER, NEVER PHONE ONLY.
// The iOS client renders the passenger block as `"\(passenger.phone) · ..."`
// (ScheduledRideSheet.swift:541) behind a single `if let passenger` whose
// presence test is the NAME (RideRequestBackend.swift:126-129 maps an
// absent-or-empty name to "no passenger at all"). Clearing the phone while
// leaving the name does not remove the passenger — it renders the block with a
// leading blank where the number used to be. Clearing both makes the whole
// branch disappear, which is the only outcome the UI renders correctly. The
// same invariant is enforced on the sweeper side in
// internal/store/ride_pruner_queries.go.
//
// Idempotent. Re-running over an already-scrubbed table selects nothing and
// touches nothing.
//
// The package is intentionally separate from internal/store so the running
// telemetry server does not pull in a destructive code path. The CLI in
// cmd/scrub-passenger-fields/ is the canonical operator entry point; it can
// also be invoked from tests.
package passengerscrub

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// querySelectRows finds every row still carrying either column.
//
// THE PREDICATE IS THE IDEMPOTENCE. An already-scrubbed row has both columns
// NULL and does not match, so a second run selects zero rows rather than
// rewriting the table.
//
// IT SELECTS ONLY THE ID. Never the values. Reading a P1 string in order to
// destroy it would put it in a Go heap and one careless log line from disk, and
// the scrub needs to know nothing about what it is erasing.
const querySelectRows = `
SELECT id
FROM go_ride_requests
WHERE passenger_name IS NOT NULL OR passenger_phone IS NOT NULL
ORDER BY id`

// queryScrubRow NULLs both columns on one row.
//
// BOTH COLUMNS IN ONE STATEMENT — see the package comment. There is no
// single-column variant of this query on purpose.
//
// It does NOT touch updated_at, and that omission is load-bearing: updated_at
// is the age boundary the MYR-447 retention sweeper keys its 365-day DELETE off
// (see store.RideRetentionDays), so bumping it here would reset every scrubbed
// ride's apparent age and defer its deletion by another full year. Postgres
// will not touch the column on its own — go_ride_requests has no BEFORE UPDATE
// trigger, only an INSERT-time default.
//
// The `IS NOT NULL` recheck makes the write, not merely the selection,
// idempotent: a row another replica scrubbed between the SELECT and this UPDATE
// reports zero rows affected instead of a redundant write.
const queryScrubRow = `
UPDATE go_ride_requests
SET passenger_name = NULL,
    passenger_phone = NULL
WHERE id = $1
  AND (passenger_name IS NOT NULL OR passenger_phone IS NOT NULL)`

// queryCountRemaining is the post-run verification: how many rows still carry
// either column. Zero is the only success.
const queryCountRemaining = `
SELECT COUNT(*)
FROM go_ride_requests
WHERE passenger_name IS NOT NULL OR passenger_phone IS NOT NULL`

// Result reports the outcome of a Run. Every field is P0 — counts only, never
// a value and never a ride id.
type Result struct {
	// RowsScanned is how many rows the SELECT returned.
	RowsScanned int
	// RowsScrubbed is how many UPDATEs actually changed a row.
	RowsScrubbed int
	// UpdateErrors is how many rows failed to scrub. Per-row failures are
	// tallied and the run CONTINUES — one locked or corrupt row must not leave
	// the rest of the column populated.
	UpdateErrors int
	// Remaining is the post-run count of rows still carrying either column.
	// Zero on a clean run; equal to RowsScanned on a dry run.
	Remaining int
	// DryRun echoes whether the run was a rehearsal.
	DryRun bool
}

// Errors reports whether any row failed mid-run. Drives the CLI's non-zero exit.
func (r Result) Errors() int { return r.UpdateErrors }

// pool is the subset of *pgxpool.Pool the scrub uses. A narrow interface keeps
// tests light.
type pool interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Scrubber runs the one-time legacy passenger-column scrub. Construct via New;
// the zero value is unusable.
type Scrubber struct {
	pool   pool
	logger *slog.Logger
	dryRun bool
}

// New returns a Scrubber bound to the given pool.
//
// dryRun makes the run REPORT WITHOUT WRITING: it counts what it would scrub
// and leaves every row intact. It exists because this operation is destructive
// and irreversible — there is no un-scrub — so an operator must be able to see
// the blast radius before authorising it.
func New(p pool, logger *slog.Logger, dryRun bool) *Scrubber {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scrubber{pool: p, logger: logger, dryRun: dryRun}
}

// Run scrubs every row carrying either passenger column and reports what it did.
//
// Returns a Result regardless of per-row failures; the caller decides whether to
// exit non-zero based on Result.Errors(). A cancelled context stops the loop at
// the next row boundary and returns what was done so far — every UPDATE is its
// own committed statement, so there is no partial state to unwind and a re-run
// simply picks up the rest.
func (s *Scrubber) Run(ctx context.Context) (Result, error) {
	res := Result{DryRun: s.dryRun}

	ids, err := s.selectRows(ctx)
	if err != nil {
		return res, err
	}
	res.RowsScanned = len(ids)

	if !s.dryRun {
		for _, id := range ids {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return res, fmt.Errorf("passengerscrub: cancelled: %w", ctxErr)
			}
			s.scrubOne(ctx, id, &res)
		}
	}

	remaining, err := CountRemaining(ctx, s.pool)
	if err != nil {
		return res, err
	}
	res.Remaining = remaining

	// P0 only: counts. Never a value, never a ride id.
	s.logger.Info("passenger_fields_scrubbed",
		slog.String("event", "passenger_fields_scrubbed"),
		slog.Bool("dry_run", s.dryRun),
		slog.Int("rows_scanned", res.RowsScanned),
		slog.Int("rows_scrubbed", res.RowsScrubbed),
		slog.Int("update_errors", res.UpdateErrors),
		slog.Int("remaining", res.Remaining),
	)
	return res, nil
}

// selectRows collects the ids of every row still carrying either column.
func (s *Scrubber) selectRows(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, querySelectRows)
	if err != nil {
		return nil, fmt.Errorf("passengerscrub: select rows: %w", err)
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr != nil {
			return nil, fmt.Errorf("passengerscrub: scan row: %w", scanErr)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("passengerscrub: iterate rows: %w", err)
	}
	return ids, nil
}

// scrubOne issues the two-column UPDATE for one row. A failure is tallied and
// logged but does NOT abort the run — the whole point is to empty the column,
// and stopping at the first stubborn row would leave the rest populated.
//
// The log line names the row id and nothing else. The id is P0; the values are
// P1 and are never read, let alone logged.
func (s *Scrubber) scrubOne(ctx context.Context, id string, res *Result) {
	tag, err := s.pool.Exec(ctx, queryScrubRow, id)
	if err != nil {
		res.UpdateErrors++
		s.logger.Warn("passengerscrub: scrub failed",
			slog.String("ride_request_id", id),
			slog.String("error", err.Error()))
		return
	}
	if tag.RowsAffected() > 0 {
		res.RowsScrubbed++
	}
}

// CountRemaining reports how many rows still carry either passenger column.
//
// Exported because it is the verification an operator runs after the fact, and
// because a future gauge would want it without owning the rest of the Scrubber.
func CountRemaining(ctx context.Context, p pool) (int, error) {
	var n int
	if err := p.QueryRow(ctx, queryCountRemaining).Scan(&n); err != nil {
		return 0, fmt.Errorf("passengerscrub: count remaining: %w", err)
	}
	return n, nil
}
