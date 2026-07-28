// Binary cleanup-stuck-drives deletes orphaned Drive rows that the
// telemetry server's drive state machine never completed -- rows where
// "endTime" IS NULL and "startTime" is older than the configured age
// threshold (default 24h).
//
// Background (MYR-139 R3b): before the MYR-139 R3a watchdog fix, Tesla
// going silent at vehicle park left drives open in memory and the
// Drive row in Postgres with endTime/distanceMiles/durationMinutes
// unwritten. The R3a fix prevents new orphans; this one-shot script
// cleans up the pre-existing orphans.
//
// Why a Go script and not a SQL migration:
//
//	"Drive" is Prisma-owned. Contract-guard rule CG-DL-9 (see
//	docs/contracts/data-lifecycle.md §7) forbids any SQL file under
//	internal/store/migrations/ from referencing Prisma-owned tables.
//	Runtime application queries against Prisma-owned tables are
//	permitted -- this script is one such query.
//
// Configuration is env-driven:
//
//	DATABASE_URL                          Postgres connection string (required)
//	CLEANUP_STUCK_DRIVES_MIN_AGE          Optional. Go duration string for
//	                                      the age threshold. Default: 24h.
//	                                      Only drives with startTime older
//	                                      than NOW() - MIN_AGE are touched.
//	CLEANUP_STUCK_DRIVES_DRY_RUN          Optional. "true" to count matching
//	                                      rows without deleting. Default: false.
//	DATABASE_DISABLE_PREPARED_STATEMENTS  "true" for PgBouncer (Supabase 6543);
//	                                      auto-detected when URL contains :6543.
//
// Exit codes:
//
//	0  success (any number of rows, including zero)
//	1  delete operation returned an error after open
//	2  fatal startup error (env, DB open, ping)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	exitOK         = 0
	exitRunError   = 1
	exitFatalSetup = 2

	defaultMinAge = 24 * time.Hour

	// deleteStmt removes orphan Drive rows older than the cutoff. The
	// "endTime IS NULL" predicate is the precise definition of "stuck"
	// produced by the pre-MYR-139-R3a state machine: the start INSERT
	// ran, but the completion UPDATE never did. The startTime cutoff
	// avoids racing with drives that are currently active in the
	// running telemetry-server (which has its own in-memory state and
	// will fire drive.ended via the watchdog within EndDebounce).
	deleteStmt = `DELETE FROM "Drive"
		WHERE "endTime" IS NULL
		  AND "startTime" < NOW() - $1::interval`

	// countStmt mirrors the WHERE clause of deleteStmt for dry-runs.
	countStmt = `SELECT COUNT(*) FROM "Drive"
		WHERE "endTime" IS NULL
		  AND "startTime" < NOW() - $1::interval`
)

func main() {
	os.Exit(run())
}

// run is the testable seam: separates os.Exit from the work loop so a
// future test can drive it without process-level side effects.
func run() int {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	minAge, err := loadMinAge()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cleanup-stuck-drives: %s\n", err)
		return exitFatalSetup
	}
	dryRun := os.Getenv("CLEANUP_STUCK_DRIVES_DRY_RUN") == "true"

	pool, err := openPool(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cleanup-stuck-drives: %s\n", err)
		return exitFatalSetup
	}
	defer pool.Close()

	logger.Info("cleanup-stuck-drives starting",
		slog.Duration("min_age", minAge),
		slog.Bool("dry_run", dryRun),
	)

	deleted, runErr := cleanupOrphans(ctx, pool, minAge, dryRun)

	report := struct {
		MinAge  string `json:"minAge"`
		DryRun  bool   `json:"dryRun"`
		RowsHit int64  `json:"rowsHit"`
		Error   string `json:"error,omitempty"`
	}{
		MinAge:  minAge.String(),
		DryRun:  dryRun,
		RowsHit: deleted,
	}
	if runErr != nil {
		report.Error = runErr.Error()
	}

	// JSON to stdout so an operator pipeline can parse the result.
	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		fmt.Fprintf(os.Stderr, "cleanup-stuck-drives: write report: %s\n", err)
		// Don't override a run-error exit with a stdout encoding failure.
	}

	switch {
	case runErr != nil && !errors.Is(runErr, context.Canceled):
		return exitRunError
	default:
		return exitOK
	}
}

// loadMinAge parses CLEANUP_STUCK_DRIVES_MIN_AGE or returns the default.
// A negative or zero duration is rejected to prevent the operator from
// accidentally widening the deletion window to "every open drive".
func loadMinAge() (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv("CLEANUP_STUCK_DRIVES_MIN_AGE"))
	if raw == "" {
		return defaultMinAge, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("CLEANUP_STUCK_DRIVES_MIN_AGE: %w", err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("CLEANUP_STUCK_DRIVES_MIN_AGE must be > 0, got %s", d)
	}
	return d, nil
}

// openPool builds a pgxpool from DATABASE_URL using the same
// PgBouncer-aware logic as the server's openDB helper. The pool is
// sized at 2 because this binary executes a single statement and is
// not expected to be invoked concurrently with itself.
func openPool(ctx context.Context) (*pgxpool.Pool, error) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	disablePrep := strings.Contains(url, ":6543") ||
		os.Getenv("DATABASE_DISABLE_PREPARED_STATEMENTS") == "true"
	if disablePrep {
		cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// cleanupOrphans runs the count (dry-run) or the delete (default) and
// returns the number of rows touched. The interval is passed to Postgres
// as a string so the same parameter type works under both prepared and
// simple-protocol query modes.
func cleanupOrphans(ctx context.Context, pool *pgxpool.Pool, minAge time.Duration, dryRun bool) (int64, error) {
	interval := intervalString(minAge)

	if dryRun {
		var n int64
		if err := pool.QueryRow(ctx, countStmt, interval).Scan(&n); err != nil {
			return 0, fmt.Errorf("count orphans: %w", err)
		}
		return n, nil
	}

	tag, err := pool.Exec(ctx, deleteStmt, interval)
	if err != nil {
		return 0, fmt.Errorf("delete orphans: %w", err)
	}
	return tag.RowsAffected(), nil
}

// intervalString formats a Go duration for Postgres's INTERVAL parser
// using seconds. We avoid PG's "1 day" notation because fractional days
// don't survive round-trip, and the seconds form is unambiguous for any
// minAge value an operator might supply.
func intervalString(d time.Duration) string {
	return fmt.Sprintf("%d seconds", int64(d.Seconds()))
}
