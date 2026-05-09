package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	// Register the pgx/v5 database driver for golang-migrate.
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
)

// migrationFiles embeds all SQL migration files from the migrations/
// subdirectory into the binary at compile time. This keeps the binary
// self-contained: no external migration files are needed at runtime.
//
// Naming convention enforced by contract-guard rule CG-DL-9:
//   - Go-owned tables MUST be prefixed "_telemetry_" or "go_"
//   - Migration SQL MUST NOT reference Prisma-owned table names
//
// See docs/architecture/migrations.md for the coexistence rule.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// RunMigrations applies all pending golang-migrate migrations embedded in
// the binary. It is called once at startup, after the connection pool is
// established and before any handler or repository is bound.
//
// Behaviour:
//   - On success (new migrations applied): logs the outcome and returns nil.
//   - On migrate.ErrNoChange (already up-to-date): treated as success, returns nil.
//   - On any other error: returns a wrapped error; the caller MUST fail fast.
//
// The dbURL must be a postgres:// connection string compatible with pgx.
// It may include query parameters (e.g. sslmode=disable). The function
// opens a dedicated single connection for migrations and does not borrow
// from the application pool, so it is safe to call before pool warmup.
func RunMigrations(ctx context.Context, dbURL string, logger *slog.Logger) error {
	logger.Info("running database migrations")

	src, err := iofs.New(migrationFiles, "migrations")
	if err != nil {
		return fmt.Errorf("store.RunMigrations: open embedded migration source: %w", err)
	}

	// golang-migrate's pgx/v5 driver accepts a "pgx5://" scheme. We
	// translate a bare postgres:// URL so callers don't need to know the
	// internal scheme convention.
	driverURL := pgxMigrateURL(dbURL)

	m, err := migrate.NewWithSourceInstance("iofs", src, driverURL)
	if err != nil {
		return fmt.Errorf("store.RunMigrations: create migrator: %w", err)
	}
	defer func() {
		srcErr, dbErr := m.Close()
		if srcErr != nil {
			logger.Warn("migration source close error", slog.Any("error", srcErr))
		}
		if dbErr != nil {
			logger.Warn("migration db close error", slog.Any("error", dbErr))
		}
	}()

	if err := m.Up(); err != nil {
		if errors.Is(err, migrate.ErrNoChange) {
			logger.Info("database migrations: already up-to-date")
			return nil
		}
		return fmt.Errorf("store.RunMigrations: apply: %w", err)
	}

	version, dirty, verErr := m.Version()
	if verErr == nil {
		logger.Info("database migrations applied",
			slog.Uint64("version", uint64(version)),
			slog.Bool("dirty", dirty),
		)
	} else {
		logger.Info("database migrations applied")
	}

	return nil
}

// pgxMigrateURL translates a standard postgres:// URL to the pgx5://
// scheme expected by the golang-migrate pgx/v5 database driver. If the
// URL already carries the pgx5:// scheme it is returned unchanged.
func pgxMigrateURL(url string) string {
	if len(url) >= 7 && url[:7] == "pgx5://" {
		return url
	}
	// Replace leading "postgres://" or "postgresql://" with "pgx5://".
	for _, prefix := range []string{"postgresql://", "postgres://"} {
		if len(url) >= len(prefix) && url[:len(prefix)] == prefix {
			return "pgx5://" + url[len(prefix):]
		}
	}
	// Unrecognised scheme -- return as-is and let the driver surface the error.
	return url
}
