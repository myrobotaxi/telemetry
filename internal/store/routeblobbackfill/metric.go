package routeblobbackfill

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// PlaintextGauge is the running-server companion to the backfill CLI:
// it periodically counts how many Vehicle / Drive rows still hold a
// plaintext route-blob without ciphertext, per column, and exposes the
// count on `/metrics` as `route_blob_plaintext_remaining_total{column=...}`.
//
// Operators alert on `gauge > 0` once the rollout is supposed to be
// done. Each Run also surfaces the same value to stdout so a CLI run
// and a /metrics scrape never disagree.
type PlaintextGauge struct {
	gauge    *prometheus.GaugeVec
	pool     *pgxpool.Pool
	interval time.Duration
	logger   *slog.Logger
}

// NewPlaintextGauge registers the gauge on reg and returns a runnable
// PlaintextGauge. The metric name is intentionally NOT placed under
// the "telemetry" namespace — this is a cross-repo migration health
// signal that should be discoverable by any operator scraping the
// process, regardless of which subsystem is reporting.
func NewPlaintextGauge(reg prometheus.Registerer, pool *pgxpool.Pool, interval time.Duration, logger *slog.Logger) *PlaintextGauge {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "route_blob_plaintext_remaining_total",
		Help: "Vehicle / Drive rows where the plaintext route blob is populated but the *Enc shadow is NULL — i.e. route polylines not yet encrypted at rest. Drains to 0 once the MYR-64 dual-write rollout backfills every row.",
	}, []string{"column"})
	reg.MustRegister(g)

	for _, col := range Columns {
		g.WithLabelValues(col.Plaintext).Set(0)
	}

	return &PlaintextGauge{
		gauge:    g,
		pool:     pool,
		interval: interval,
		logger:   logger,
	}
}

// Run starts the periodic gauge update loop. Blocks until ctx is
// cancelled. Call this in a goroutine. An immediate refresh runs
// before the first tick so a freshly started server doesn't expose
// stale zeros.
func (p *PlaintextGauge) Run(ctx context.Context) {
	p.refreshOnce(ctx)
	if p.interval <= 0 {
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.refreshOnce(ctx)
		}
	}
}

// Set updates the gauge with a pre-computed map (one entry per
// plaintext column). Used by the backfill CLI immediately after a
// Run() so the post-run metric value matches the stdout report.
func (p *PlaintextGauge) Set(remaining map[string]int) {
	for _, col := range Columns {
		v, ok := remaining[col.Plaintext]
		if !ok {
			continue
		}
		p.gauge.WithLabelValues(col.Plaintext).Set(float64(v))
	}
}

// refreshOnce queries Postgres and updates the gauge. Logs a Warn on
// query failure but doesn't escalate — a broken metric must not crash
// the running server.
func (p *PlaintextGauge) refreshOnce(ctx context.Context) {
	remaining, err := CountPlaintextRemaining(ctx, p.pool)
	if err != nil {
		if p.logger != nil {
			p.logger.Warn("route-blob plaintext-remaining query failed",
				slog.String("error", err.Error()))
		}
		return
	}
	p.Set(remaining)
}
