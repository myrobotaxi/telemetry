// Package store provides PostgreSQL persistence for vehicle telemetry and
// drive records. It wraps pgxpool for connection management and implements
// the repository pattern for the Prisma-owned "Vehicle" and "Drive" tables.
package store

import (
	"context"
	"fmt"
	"log/slog"
	"math"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tnando/my-robo-taxi-telemetry/internal/config"
)

// DB manages the PostgreSQL connection pool and provides health checks.
type DB struct {
	pool    *pgxpool.Pool
	logger  *slog.Logger
	metrics Metrics
}

// NewDB connects to PostgreSQL, validates the connection, and returns a DB.
// It fails fast if the database is unreachable at startup.
func NewDB(ctx context.Context, cfg config.DatabaseConfig, logger *slog.Logger, metrics Metrics) (*DB, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("store.NewDB: parse config: %w", err)
	}

	if cfg.MaxConns > 0 && cfg.MaxConns <= math.MaxInt32 {
		poolCfg.MaxConns = int32(cfg.MaxConns) // #nosec G115 -- bounds checked above
	}
	if cfg.MinConns > 0 && cfg.MinConns <= math.MaxInt32 {
		poolCfg.MinConns = int32(cfg.MinConns) // #nosec G115 -- bounds checked above
	}

	// Disable prepared statement caching for PgBouncer transaction pooling
	// (Supabase port 6543). Prepared statements are per-connection state that
	// PgBouncer does not track when rotating connections between queries.
	if cfg.DisablePreparedStatements {
		poolCfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("store.NewDB: create pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store.NewDB: ping: %w", err)
	}

	logger.Info("database connection established",
		slog.Int("max_conns", int(poolCfg.MaxConns)),
		slog.Int("min_conns", int(poolCfg.MinConns)),
		slog.Bool("simple_protocol", cfg.DisablePreparedStatements),
	)

	return &DB{
		pool:    pool,
		logger:  logger,
		metrics: metrics,
	}, nil
}

// Ping tests the database connection. Used by the /readyz health check.
func (db *DB) Ping(ctx context.Context) error {
	if err := db.pool.Ping(ctx); err != nil {
		return fmt.Errorf("store.DB.Ping: %w", err)
	}
	return nil
}

// Close gracefully closes the connection pool.
func (db *DB) Close() {
	db.pool.Close()
	db.logger.Info("database connection pool closed")
}

// Pool returns the underlying pgxpool.Pool for use by repositories.
func (db *DB) Pool() *pgxpool.Pool {
	return db.pool
}

// CollectPoolStats reads the current pool statistics and reports them
// via the Metrics interface. Call this periodically from a ticker.
func (db *DB) CollectPoolStats() {
	stat := db.pool.Stat()
	db.metrics.SetPoolStats(stat.AcquiredConns(), stat.IdleConns(), stat.TotalConns())
}
