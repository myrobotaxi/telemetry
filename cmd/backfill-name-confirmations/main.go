// Binary backfill-name-confirmations is MYR-583's deploy-time data step. It does
// two things, in one transaction, once:
//
//  1. SCRUBS the legacy web onboarding's literal 'Tesla User' placeholder out of
//     `"User"."name"` (expected: exactly 1 row in production — the 2026-03
//     web-era account the old react-frontend auth seeded).
//  2. BACKFILLS `go_profile_name_confirmations` for the accounts that renamed
//     through `PATCH /api/users/me` between its deploy (2026-08-17 ~07:00Z) and
//     migration 0042 creating the table.
//
// WHY A BINARY AND NOT A MIGRATION. Both statements name the Prisma-owned
// `"User"` table, and contract-guard rule CG-DL-9 forbids any file under
// internal/store/migrations/ from naming it at all. An application-runtime
// statement is the sanctioned class; see internal/store/nameconfirmbackfill for
// the full argument and docs/contracts/data-lifecycle.md §1.4.2 for the carve-out.
//
// RUN -dry-run FIRST. It reports exactly what would change and writes nothing
// (it counts inside a transaction and rolls back). Expect
// placeholdersScrubbed = 1 against production; anything larger means the
// placeholder reached accounts nobody knew about, and is worth understanding
// before clearing them.
//
// AUDITED. Every scrubbed account gets a `profile_name_placeholder_scrubbed`
// AuditLog row, written in the same transaction as the scrub, so an operator
// changing somebody's name column out of band leaves a record rather than a
// terminal scrollback. A real run REFUSES to proceed if the audit writer is
// missing; a dry run needs none.
//
// IDEMPOTENT. Re-running scrubs nothing and inserts nothing.
//
// ORDER RELATIVE TO THE SERVER DEPLOY. Migration 0042 must have run (the server
// applies it at startup), because the backfill inserts into the table it creates.
// Beyond that the ordering is free: the confirmation gate is already live and
// simply treats every un-backfilled account as unconfirmed, which is the state
// the client's name prompt resolves.
//
// Configuration is env-driven, matching the running telemetry-server:
//
//	DATABASE_URL                  Postgres connection string (required)
//	DATABASE_DISABLE_PREPARED_STATEMENTS
//	                              "true" for PgBouncer (Supabase 6543);
//	                              auto-detected when the URL contains :6543
//
// No ENCRYPTION_KEY is needed or read: nothing here is encrypted, and the scrub
// never reads a name — the value it clears is the literal in its own predicate.
//
// Usage:
//
//	backfill-name-confirmations -dry-run   # rehearse; writes nothing
//	backfill-name-confirmations            # scrub + backfill for real
//
// Exit codes:
//
//	0  success
//	2  fatal error (config, DB, or a failed statement — nothing was committed)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/store/nameconfirmbackfill"
)

const (
	exitOK    = 0
	exitFatal = 2
)

// Pool bounds for a binary whose whole run is one transaction. Typed int32 to
// match pgxpool directly, so there is no bounds-checked conversion to justify.
const (
	poolMaxConns int32 = 2
	poolMinConns int32 = 1
)

func main() {
	os.Exit(run())
}

// run is the testable seam — separated so a test can drive it without os.Exit.
func run() int {
	dryRun := flag.Bool("dry-run", false,
		"report what would change and write nothing. Run this first.")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := openPool(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backfill-name-confirmations: %s\n", err)
		return exitFatal
	}
	defer pool.Close()

	runner := nameconfirmbackfill.New(pool, logger, *dryRun).
		WithAuditor(store.NewProfileNamePlaceholderAuditor())

	res, runErr := runner.Run(ctx)

	report := struct {
		DryRun                  bool   `json:"dryRun"`
		PlaceholdersScrubbed    int    `json:"placeholdersScrubbed"`
		ConfirmationsBackfilled int    `json:"confirmationsBackfilled"`
		Error                   string `json:"error,omitempty"`
	}{
		DryRun:                  res.DryRun,
		PlaceholdersScrubbed:    res.PlaceholdersScrubbed,
		ConfirmationsBackfilled: res.ConfirmationsBackfilled,
	}
	if runErr != nil {
		report.Error = runErr.Error()
	}
	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		fmt.Fprintf(os.Stderr, "backfill-name-confirmations: write report: %s\n", err)
	}

	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		return exitFatal
	}
	return exitOK
}

// openPool builds a pgxpool from DATABASE_URL using the same PgBouncer-aware
// logic as the server's openDB helper.
func openPool(ctx context.Context) (*pgxpool.Pool, error) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return nil, errors.New("DATABASE_URL is required")
	}
	cfg := config.DatabaseConfig{
		URL: url,
		DisablePreparedStatements: strings.Contains(url, ":6543") ||
			os.Getenv("DATABASE_DISABLE_PREPARED_STATEMENTS") == "true",
	}
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	// The bounds are APPLIED here rather than carried on the config struct, so
	// they cannot read as intent while doing nothing. A one-off binary that opens
	// ONE transaction has no use for pgx's default pool (GOMAXPROCS connections)
	// against a production database the server is also connected to.
	poolCfg.MaxConns = poolMaxConns
	poolCfg.MinConns = poolMinConns
	if cfg.DisablePreparedStatements {
		poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}
