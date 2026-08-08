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

// Ride retention wiring (MYR-447).
//
// One moving part, the same shape as the drive sweep in wiring_retention.go: a
// daily timer that deletes terminal ride requests past the 365-day window and
// scrubs the deprecated booked-for-passenger columns at terminal + 30 days.
// Started with a bare `go Run(ctx)` and no drain — the sweep spawns no
// goroutines, and each batch is its own transaction, so shutdown mid-pass rolls
// back the batch in flight and the next pass resumes from the same oldest-first
// position.

// startRidePruner launches the daily ride retention sweep.
//
// The RIDE_RETENTION_PRUNER_ENABLED switch defaults ON, and is deliberately
// separate from the drive sweep's. Leaving it off is not a neutral degraded
// mode: it suspends both a record-retention promise and the removal of a
// deprecated feature's PII, for as long as it stays off. It is a switch for
// stopping a MISBEHAVING sweep, not for deferring one.
func startRidePruner(
	ctx context.Context,
	cfg *config.Config,
	reg prometheus.Registerer,
	pool *pgxpool.Pool,
	logger *slog.Logger,
) {
	log := logger.With(slog.String("component", "ride-retention-pruner"))
	pruner := retention.NewRidePruner(
		&ridePrunerAdapter{pruner: store.NewRidePruner(pool, log)},
		retention.Config{Enabled: cfg.RidePrunerEnabled()},
		retention.NewPrometheusRideMetrics(reg),
		log,
	)
	go pruner.Run(ctx)
}

// ridePrunerAdapter adapts store.RidePruner onto retention.RideStore, so
// internal/retention never imports internal/store — and, more to the point,
// never sees either retention window, which live on the store side as
// compile-time constants (CG-DL-4).
type ridePrunerAdapter struct {
	pruner *store.RidePruner
}

func (a *ridePrunerAdapter) PruneBatch(ctx context.Context, batchSize int) (retention.RideBatchOutcome, error) {
	res, err := a.pruner.PruneBatch(ctx, batchSize)
	if err != nil {
		return retention.RideBatchOutcome{}, fmt.Errorf("ride retention: prune batch: %w", err)
	}
	return retention.RideBatchOutcome{
		RidesDeleted:       res.RidesDeleted,
		PassengersScrubbed: res.PassengersScrubbed,
		AuditRows:          res.AuditRows,
		Exhausted:          res.Exhausted,
	}, nil
}
