package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/cryptox"
	"github.com/myrobotaxi/telemetry/internal/store"
)

// loadEncryptor builds a cryptox.Encryptor from the same env-var schema
// the server uses (ENCRYPTION_KEY or the versioned shape). Required for
// every helper that constructs an AccountRepo: the dual-write contract
// in MYR-62 enforces a non-nil encryptor.
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

// newAccountRepo is the canonical constructor used by every ops
// subcommand that needs to read or write Account tokens. Centralizing it
// here keeps the encryption foundation in one place across auth, fleet,
// and link subcommands.
func newAccountRepo(db *store.DB) (*store.AccountRepo, error) {
	enc, err := loadEncryptor()
	if err != nil {
		return nil, err
	}
	return store.NewAccountRepo(db.Pool(), enc), nil
}

// newLogger returns a text-handler slog logger writing to stderr so
// normal JSON output on stdout remains machine-parseable.
func newLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// openDB builds a DatabaseConfig from environment variables and opens a
// connection pool. The caller is responsible for closing the returned DB.
// DATABASE_URL is required. PgBouncer transaction pooling (Supabase port
// 6543) is auto-detected by the presence of :6543 in the URL, matching
// the server's handling of DATABASE_DISABLE_PREPARED_STATEMENTS.
func openDB(ctx context.Context, logger *slog.Logger) (*store.DB, error) {
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

	db, err := store.NewDB(ctx, cfg, logger, store.NoopMetrics{})
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	return db, nil
}

// writeJSON marshals v as indented JSON and writes it to w followed by a
// newline. Returns an error if marshalling or writing fails.
func writeJSON(w io.Writer, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write json: %w", err)
	}
	return nil
}

// requireFlag returns the value of flag if non-empty; otherwise returns a
// descriptive usage error.
func requireFlag(name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("--%s is required", name)
	}
	return nil
}
