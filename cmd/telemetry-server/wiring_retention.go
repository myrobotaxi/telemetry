package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/retention"
	"github.com/myrobotaxi/telemetry/internal/store"
)

// Drive retention wiring (MYR-439, NFR-3.27).
//
// One moving part: a daily timer that deletes drives past the 365-day window.
// Started with a bare `go Run(ctx)` and no drain, matching every other
// timer-driven worker in this process — the sweep spawns no goroutines, and
// each batch is its own transaction, so shutdown mid-pass rolls back the batch
// in flight and the next pass resumes from the same oldest-first position.

// startDrivePruner launches the daily drive retention sweep.
//
// The DRIVE_RETENTION_PRUNER_ENABLED switch defaults ON. Unlike most kill
// switches here, leaving this one off is not a neutral degraded mode: the
// 365-day window is a promise made to owners in the privacy policy, so the
// sweep being off means the promise is being broken for as long as it stays
// off. It is a switch for stopping a MISBEHAVING sweep, not for deferring one.
func startDrivePruner(
	ctx context.Context,
	cfg *config.Config,
	reg prometheus.Registerer,
	pool *pgxpool.Pool,
	logger *slog.Logger,
) {
	log := logger.With(slog.String("component", "drive-retention-pruner"))
	pruner := retention.NewPruner(
		&drivePrunerAdapter{pruner: store.NewDrivePruner(pool, log)},
		retention.Config{Enabled: cfg.DrivePrunerEnabled()},
		retention.NewPrometheusMetrics(reg),
		log,
	)
	go pruner.Run(ctx)
}

// drivePrunerAdapter adapts store.DrivePruner onto retention.DriveStore, so
// internal/retention never imports internal/store — and, more to the point,
// never sees the retention window, which lives on the store side as a
// compile-time constant (CG-DL-4).
type drivePrunerAdapter struct {
	pruner *store.DrivePruner
}

func (a *drivePrunerAdapter) PruneBatch(ctx context.Context, batchSize int) (retention.BatchOutcome, error) {
	res, err := a.pruner.PruneBatch(ctx, batchSize)
	if err != nil {
		return retention.BatchOutcome{}, fmt.Errorf("drive retention: prune batch: %w", err)
	}
	return retention.BatchOutcome{
		DrivesDeleted: res.DrivesDeleted,
		AuditRows:     res.AuditRows,
		Exhausted:     res.Exhausted,
	}, nil
}
