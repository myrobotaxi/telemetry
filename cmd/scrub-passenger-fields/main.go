// Binary scrub-passenger-fields NULLs the two deprecated booked-for-passenger
// columns on go_ride_requests across the WHOLE table, once (MYR-447):
//
//   - go_ride_requests.passenger_name  (P1 plaintext)
//   - go_ride_requests.passenger_phone (P1 plaintext)
//
// The book-for-someone-else feature was removed from the app in MYR-382 and no
// live surface renders a passenger, so these values have no reader. The MYR-447
// retention sweeper scrubs them at terminal + 30 days going forward; this
// binary clears the legacy backlog now, including rows the sweeper would never
// reach because their ride is still open.
//
// DESTRUCTIVE AND IRREVERSIBLE — there is no un-scrub. RUN -dry-run FIRST: it
// reports exactly how many rows would be affected and writes nothing.
//
// It always scrubs BOTH columns together, never phone alone; see
// internal/store/passengerscrub for why a name-without-phone row is a broken
// render rather than a partial success.
//
// Configuration is env-driven, matching the running telemetry-server:
//
//	DATABASE_URL                  Postgres connection string (required)
//	DATABASE_DISABLE_PREPARED_STATEMENTS
//	                              "true" for PgBouncer (Supabase 6543);
//	                              auto-detected when URL contains :6543
//
// No ENCRYPTION_KEY is needed or read: these two columns are plaintext, and the
// scrub never decrypts, reads or logs a value — it only NULLs them.
//
// Usage:
//
//	scrub-passenger-fields -dry-run   # rehearse; writes nothing
//	scrub-passenger-fields            # scrub for real
//
// Exit codes:
//
//	0  success and no row failed
//	1  any row failed to scrub; details on stderr
//	2  fatal startup error (config, DB)
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
	"github.com/myrobotaxi/telemetry/internal/store/passengerscrub"
)

// exitCodes mirror the package comment above.
const (
	exitOK         = 0
	exitRowErrors  = 1
	exitFatalSetup = 2
)

func main() {
	os.Exit(run())
}

// run is the testable seam — separated so a test can call it with a custom env
// without going through os.Exit.
func run() int {
	dryRun := flag.Bool("dry-run", false,
		"report what would be scrubbed and write nothing. Run this first: the scrub is irreversible.")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := openPool(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "scrub-passenger-fields: %s\n", err)
		return exitFatalSetup
	}
	defer pool.Close()

	res, runErr := passengerscrub.New(pool, logger, *dryRun).Run(ctx)

	report := struct {
		DryRun       bool   `json:"dryRun"`
		RowsScanned  int    `json:"rowsScanned"`
		RowsScrubbed int    `json:"rowsScrubbed"`
		UpdateErrors int    `json:"updateErrors"`
		Remaining    int    `json:"remaining"`
		Error        string `json:"error,omitempty"`
	}{
		DryRun:       res.DryRun,
		RowsScanned:  res.RowsScanned,
		RowsScrubbed: res.RowsScrubbed,
		UpdateErrors: res.UpdateErrors,
		Remaining:    res.Remaining,
	}
	if runErr != nil {
		report.Error = runErr.Error()
	}

	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		fmt.Fprintf(os.Stderr, "scrub-passenger-fields: write report: %s\n", err)
	}

	switch {
	case res.Errors() > 0:
		return exitRowErrors
	case runErr != nil && !errors.Is(runErr, context.Canceled):
		return exitFatalSetup
	default:
		return exitOK
	}
}

// openPool builds a pgxpool from DATABASE_URL using the same PgBouncer-aware
// logic as the server's openDB helper.
func openPool(ctx context.Context) (*pgxpool.Pool, error) {
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return nil, fmt.Errorf("DATABASE_URL is required")
	}
	cfg := config.DatabaseConfig{
		URL:      url,
		MaxConns: 2,
		MinConns: 1,
		DisablePreparedStatements: strings.Contains(url, ":6543") ||
			os.Getenv("DATABASE_DISABLE_PREPARED_STATEMENTS") == "true",
	}
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
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
