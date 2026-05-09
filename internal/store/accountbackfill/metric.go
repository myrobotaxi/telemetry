package accountbackfill

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
)

// PlaintextGauge is the running-server companion to the backfill CLI:
// it periodically counts how many Tesla Account rows still hold a
// plaintext token without ciphertext, per column, and exposes the
// number on `/metrics` as `account_token_plaintext_remaining_total{column=...}`.
//
// Operators alert on (gauge > 0) once the rollout is supposed to be
// done. Each Run also surfaces the same value to stdout so a CLI run
// and a `/metrics` scrape never disagree.
type PlaintextGauge struct {
	gauge    *prometheus.GaugeVec
	pool     *pgxpool.Pool
	interval time.Duration
	logger   *slog.Logger
}

// NewPlaintextGauge registers the gauge on reg and returns a runnable
// PlaintextGauge. The metric name is intentionally NOT placed under the
// "telemetry" namespace — this number is a cross-repo migration health
// signal that should be discoverable by any operator scraping the
// process, regardless of which subsystem is reporting.
func NewPlaintextGauge(reg prometheus.Registerer, pool *pgxpool.Pool, interval time.Duration, logger *slog.Logger) *PlaintextGauge {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "account_token_plaintext_remaining_total",
		Help: "Tesla Account rows where <col> is non-NULL but <col>_enc is NULL — i.e. plaintext OAuth tokens not yet encrypted at rest. Drains to 0 once the MYR-62 dual-write rollout backfills every row.",
	}, []string{"column"})
	reg.MustRegister(g)

	// Pre-register every label so the metric is visible at /metrics
	// before the first scrape, even if the loop hasn't run yet.
	for _, col := range TokenColumns {
		g.WithLabelValues(col).Set(0)
	}

	return &PlaintextGauge{
		gauge:    g,
		pool:     pool,
		interval: interval,
		logger:   logger,
	}
}

// Run starts the periodic gauge update loop. Blocks until ctx is
// cancelled. Call this in a goroutine. An immediate refresh runs before
// the first tick so a freshly started server doesn't expose stale zeros.
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

// Set updates the gauge with a pre-computed map (one entry per column).
// Used by the backfill CLI immediately after a Run() so the post-run
// metric value matches the stdout report. The caller is responsible for
// holding a reference to the same registry (typically loaded from a
// `/metrics` endpoint or pushed via pushgateway in batch jobs).
func (p *PlaintextGauge) Set(remaining map[string]int) {
	for _, col := range TokenColumns {
		v, ok := remaining[col]
		if !ok {
			continue
		}
		p.gauge.WithLabelValues(col).Set(float64(v))
	}
}

// refreshOnce queries Postgres and updates the gauge. Logs a Warn on
// query failure but doesn't escalate: a broken metric must not crash
// the running server.
func (p *PlaintextGauge) refreshOnce(ctx context.Context) {
	remaining, err := CountPlaintextRemaining(ctx, p.pool)
	if err != nil {
		if p.logger != nil {
			p.logger.Warn("account-token plaintext-remaining query failed",
				slog.String("error", err.Error()))
		}
		return
	}
	p.Set(remaining)
}
