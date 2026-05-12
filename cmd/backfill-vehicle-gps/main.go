// Binary backfill-vehicle-gps encrypts pre-existing plaintext GPS
// coordinates in the Vehicle table into their *Enc ciphertext shadow
// columns (MYR-63 dual-write rollout). Idempotent: re-running over a
// fully migrated table is a no-op.
//
// The six columns covered, organized as three atomic-pair (lat, lng)
// pairs, mirror data-classification.md §1.3:
//
//   - latitude / longitude
//   - destinationLatitude / destinationLongitude
//   - originLatitude / originLongitude
//
// Atomic-pair invariant: a pair is encrypted only when BOTH plaintext
// halves are non-NULL — half-pair plaintext is left untouched (there's
// no atomic-pair answer for "encrypt half a pair"). The same invariant
// is enforced on the running server's write path; see
// internal/store/vehicle_gps_encryption.go.
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

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/cryptox"
	"github.com/myrobotaxi/telemetry/internal/store/vehiclegpsbackfill"
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

// run is the testable seam — separated so a future test can call it
// with a custom env without going through os.Exit.
func run() int {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := openPool(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "backfill-vehicle-gps: %s\n", err)
		return exitFatalSetup
	}
	defer pool.Close()

	enc, err := loadEncryptor()
	if err != nil {
		fmt.Fprintf(os.Stderr, "backfill-vehicle-gps: %s\n", err)
		return exitFatalSetup
	}

	bf := vehiclegpsbackfill.New(pool, enc, logger)
	res, runErr := bf.Run(ctx)

	report := struct {
		RowsScanned        int            `json:"rowsScanned"`
		PairsEncrypted     int            `json:"pairsEncrypted"`
		RowsUpdated        int            `json:"rowsUpdated"`
		EncryptErrors      int            `json:"encryptErrors"`
		UpdateErrors       int            `json:"updateErrors"`
		PlaintextRemaining map[string]int `json:"plaintextRemaining,omitempty"`
		Error              string         `json:"error,omitempty"`
	}{
		RowsScanned:    res.RowsScanned,
		PairsEncrypted: res.PairsEncrypted,
		RowsUpdated:    res.RowsUpdated,
		EncryptErrors:  res.EncryptErrors,
		UpdateErrors:   res.UpdateErrors,
	}
	if len(res.PlaintextRemaining) > 0 {
		report.PlaintextRemaining = res.PlaintextRemaining
	}
	if runErr != nil {
		report.Error = runErr.Error()
	}

	if err := json.NewEncoder(os.Stdout).Encode(report); err != nil {
		fmt.Fprintf(os.Stderr, "backfill-vehicle-gps: write report: %s\n", err)
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

// loadEncryptor mirrors the server's setupEncryption: ENCRYPTION_KEY
// (or the versioned shape) is required at startup. The CLI fails fast
// if the key isn't present so a misconfigured run can't silently no-op.
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
