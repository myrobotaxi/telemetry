// Binary purge-plaintext-columns scrubs the retired plaintext copies of
// every column the telemetry server now stores encrypted (MYR-433):
// Tesla OAuth tokens, the six Vehicle GPS columns,
// Vehicle.navRouteCoordinates, and Drive.routePoints.
//
// This is the last step of the MYR-62/63/64 encryption rollout. Run it
// AFTER the three backfills have sealed every legacy value and AFTER the
// ciphertext-only server binary is deployed. Idempotent — re-running over
// a purged database touches nothing.
//
// Every row is verified before it is scrubbed: the ciphertext sibling is
// decrypted and compared against the plaintext, and any row that cannot
// be verified is reported and left completely intact. The purge never
// destroys the only copy of a value. See internal/store/plaintextpurge.
//
// Run with -dry-run first. It performs the identical scan and
// verification and writes nothing, so the report tells you exactly what
// would be scrubbed and what is blocked.
//
// This is a command rather than a migration because every affected column
// lives on a Prisma-owned table, which Go migration SQL may not touch
// (docs/architecture/migrations.md §4.2, enforced by CG-DL-9).
//
// Configuration is env-driven, matching the running telemetry-server:
//
//	DATABASE_URL                  Postgres connection string (required)
//	ENCRYPTION_KEY                base64(32B) AES-256 key (required), OR
//	ENCRYPTION_KEY_V<N> +
//	ENCRYPTION_WRITE_VERSION      versioned-shape key set
//	DATABASE_DISABLE_PREPARED_STATEMENTS
//	                              "true" for PgBouncer (Supabase 6543);
//	                              auto-detected when URL contains :6543
//
// Flags:
//
//	-dry-run    verify and report without writing (default false)
//
// Exit codes:
//
//	0  success: nothing blocked, nothing left behind
//	1  rows were left in place (unverifiable) or a scrub write failed
//	2  fatal startup error (config, DB, key)
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
	"github.com/myrobotaxi/telemetry/internal/cryptox"
	"github.com/myrobotaxi/telemetry/internal/store/plaintextpurge"
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

// run is the testable seam — separated so a test can call it without
// going through os.Exit.
func run() int {
	dryRun := flag.Bool("dry-run", false,
		"verify and report which rows would be scrubbed, without writing")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := openPool(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "purge-plaintext-columns: %s\n", err)
		return exitFatalSetup
	}
	defer pool.Close()

	enc, err := loadEncryptor()
	if err != nil {
		fmt.Fprintf(os.Stderr, "purge-plaintext-columns: %s\n", err)
		return exitFatalSetup
	}

	res, runErr := plaintextpurge.New(pool, enc, logger).Run(ctx, *dryRun)

	writeReport(res, runErr)

	switch {
	case res.TotalBlocked() > 0 || res.UpdateErrors() > 0:
		return exitRowErrors
	case runErr != nil && !errors.Is(runErr, context.Canceled):
		return exitFatalSetup
	default:
		return exitOK
	}
}

// columnReport is the per-column JSON shape.
type columnReport struct {
	Scanned       int `json:"scanned"`
	Purged        int `json:"purged"`
	Unsealed      int `json:"unsealed"`
	Mismatched    int `json:"mismatched"`
	Undecryptable int `json:"undecryptable"`
	UpdateErrors  int `json:"updateErrors"`
	Remaining     int `json:"remaining"`
}

// writeReport emits the machine-readable run summary on stdout.
//
// `remaining` per column, and `totalRemaining`, are the numbers that
// matter: the MYR-433 acceptance bar is met when totalRemaining is 0,
// because that is the count of rows from which an operator could still
// read a token or a coordinate.
func writeReport(res plaintextpurge.Result, runErr error) {
	cols := make(map[string]columnReport, len(res.Columns))
	for label, c := range res.Columns {
		cols[label] = columnReport{
			Scanned:       c.Scanned,
			Purged:        c.Purged,
			Unsealed:      c.Unsealed,
			Mismatched:    c.Mismatched,
			Undecryptable: c.Undecryptable,
			UpdateErrors:  c.UpdateErrors,
			Remaining:     c.Remaining,
		}
	}

	report := struct {
		DryRun         bool                    `json:"dryRun"`
		Columns        map[string]columnReport `json:"columns"`
		TotalPurged    int                     `json:"totalPurged"`
		TotalBlocked   int                     `json:"totalBlocked"`
		TotalRemaining int                     `json:"totalRemaining"`
		Error          string                  `json:"error,omitempty"`
	}{
		DryRun:         res.DryRun,
		Columns:        cols,
		TotalPurged:    res.TotalPurged(),
		TotalBlocked:   res.TotalBlocked(),
		TotalRemaining: res.TotalRemaining(),
	}
	if runErr != nil {
		report.Error = runErr.Error()
	}

	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		fmt.Fprintf(os.Stderr, "purge-plaintext-columns: write report: %s\n", err)
	}
}

// openPool builds a pgxpool from DATABASE_URL using the same
// PgBouncer-aware logic as the server's openDB helper.
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

// loadEncryptor mirrors the server's setupEncryption. The key is
// mandatory: the purge decides what to destroy based on what it can
// decrypt, so running without the right key must be impossible rather
// than merely unproductive.
func loadEncryptor() (cryptox.Encryptor, error) {
	ks, err := cryptox.LoadKeySetFromEnv()
	if err != nil {
		return nil, fmt.Errorf("load encryption key: %w", err)
	}
	enc, err := cryptox.NewEncryptor(ks)
	if err != nil {
		return nil, fmt.Errorf("new encryptor: %w", err)
	}
	return enc, nil
}
