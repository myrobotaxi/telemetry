// Binary backfill-account-tokens encrypts pre-existing plaintext OAuth
// tokens in the Account table into their *_enc ciphertext columns
// (MYR-62 dual-write rollout). Idempotent: re-running over a fully
// migrated table is a no-op.
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
// Exit codes:
//
//	0  success and no row failed
//	1  any row failed to encrypt or update; details on stderr
//	2  fatal startup error (config, DB, key)
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

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tnando/my-robo-taxi-telemetry/internal/config"
	"github.com/tnando/my-robo-taxi-telemetry/internal/cryptox"
	"github.com/tnando/my-robo-taxi-telemetry/internal/store"
	"github.com/tnando/my-robo-taxi-telemetry/internal/store/accountbackfill"
)

// exitCodes mirror the package comment above. Constants rather than
// magic numbers so the operator-facing contract is one read away.
const (
	exitOK         = 0
	exitRowErrors  = 1
	exitFatalSetup = 2
)

func main() {
	code := run()
	os.Exit(code)
}

// run is the testable seam — separated so a future test can call it with
// a custom env without going through os.Exit.
func run() int {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := openPool(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backfill-account-tokens: %s\n", err)
		return exitFatalSetup
	}
	defer pool.Close()

	enc, err := loadEncryptor()
	if err != nil {
		fmt.Fprintf(os.Stderr, "backfill-account-tokens: %s\n", err)
		return exitFatalSetup
	}

	// Verify the AccountRepo path agrees with the encryptor we just
	// loaded — fail loud if the dual-write contract is broken (e.g. a
	// nil encryptor sneaking through). This is the same constructor the
	// running server uses.
	_ = store.NewAccountRepo(pool, enc)

	bf := accountbackfill.New(pool, enc, logger)
	res, runErr := bf.Run(ctx)

	report := struct {
		RowsScanned        int            `json:"rowsScanned"`
		ColumnsEncrypted   int            `json:"columnsEncrypted"`
		RowsUpdated        int            `json:"rowsUpdated"`
		EncryptErrors      int            `json:"encryptErrors"`
		UpdateErrors       int            `json:"updateErrors"`
		PlaintextRemaining map[string]int `json:"plaintextRemaining"`
		Error              string         `json:"error,omitempty"`
	}{
		RowsScanned:        res.RowsScanned,
		ColumnsEncrypted:   res.ColumnsEncrypted,
		RowsUpdated:        res.RowsUpdated,
		EncryptErrors:      res.EncryptErrors,
		UpdateErrors:       res.UpdateErrors,
		PlaintextRemaining: res.PlaintextRemaining,
	}
	if runErr != nil {
		report.Error = runErr.Error()
	}

	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		fmt.Fprintf(os.Stderr, "backfill-account-tokens: write report: %s\n", err)
		// don't override a row-error exit with a stdout encoding failure
	}

	switch {
	case res.Errors() > 0:
		return exitRowErrors
	case runErr != nil && !errors.Is(runErr, context.Canceled):
		// Non-row failures (e.g. plaintext-remaining query failed) are
		// reported as setup-level errors so an alerting pipeline can
		// distinguish "can't tell if rollout is done" from "rows failed".
		return exitFatalSetup
	default:
		return exitOK
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

// loadEncryptor mirrors the server's setupEncryption: ENCRYPTION_KEY (or
// the versioned shape) is required at startup. The CLI fails fast if the
// key isn't present so a misconfigured run can't silently no-op.
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
