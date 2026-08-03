package retention

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the sweep's observability surface (data-lifecycle.md §5.7).
// Declared at the consumer site; NoopMetrics is the test/default
// implementation and PrometheusMetrics the production one.
type Metrics interface {
	// AddDrivesDeleted records drives destroyed by one batch.
	AddDrivesDeleted(n int)
	// IncBatch records one successfully committed batch.
	IncBatch()
	// IncBatchError records one batch that failed all its attempts.
	IncBatchError()
	// ObserveRunDuration records the wall-clock time of one pass.
	ObserveRunDuration(d time.Duration)
	// SetLastSuccess records when a pass last finished without a batch error.
	SetLastSuccess(t time.Time)
}

// NoopMetrics discards every observation. The default when no registry is wired.
type NoopMetrics struct{}

func (NoopMetrics) AddDrivesDeleted(int)             {}
func (NoopMetrics) IncBatch()                        {}
func (NoopMetrics) IncBatchError()                   {}
func (NoopMetrics) ObserveRunDuration(time.Duration) {}
func (NoopMetrics) SetLastSuccess(time.Time)         {}

// PrometheusMetrics is the live collector set for the retention sweep.
type PrometheusMetrics struct {
	drivesDeleted prometheus.Counter
	batches       prometheus.Counter
	batchErrors   prometheus.Counter
	runDuration   prometheus.Histogram
	lastSuccess   prometheus.Gauge
}

// NewPrometheusMetrics builds and registers the sweep's collectors.
func NewPrometheusMetrics(reg prometheus.Registerer) *PrometheusMetrics {
	m := &PrometheusMetrics{
		drivesDeleted: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "telemetry_pruner_drives_deleted_total",
			Help: "Drive rows deleted by the 365-day retention sweep, cumulative. " +
				"EXPECT ZERO UNTIL 2027-03-08: the oldest production drive was created " +
				"2026-03-08, so no row is eligible before then and a flat line is the " +
				"correct reading, not a stalled sweep. Use " +
				"telemetry_pruner_last_success_timestamp_seconds to tell 'nothing to do' " +
				"from 'not running'. After that date, expect a modest daily rate.",
		}),
		batches: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "telemetry_pruner_batches_processed_total",
			Help: "Retention batches committed, cumulative. Divided by drives_deleted " +
				"this gives the average batch fill; a ratio far below the batch size " +
				"means the sweep is keeping up.",
		}),
		batchErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "telemetry_pruner_batch_errors_total",
			Help: "Retention batches that failed every attempt and ended their pass. " +
				"Alert on any increase: the failed batch is retried next run, but a " +
				"persistent error means drives are aging past the promised window.",
		}),
		runDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "telemetry_pruner_run_duration_seconds",
			Help: "Wall-clock duration of one retention pass. Buckets span sub-second " +
				"(nothing to do — the expected case until 2027-03-08) to tens of " +
				"minutes (a large backlog, should it ever accumulate).",
			Buckets: []float64{0.01, 0.1, 1, 10, 60, 300, 900, 3600},
		}),
		lastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "telemetry_pruner_last_success_timestamp_seconds",
			Help: "Unix timestamp of the last retention pass that completed without a " +
				"batch error. Alert when now() minus this exceeds ~48h: the retention " +
				"promise is a privacy commitment, so a silently stalled sweep is a " +
				"compliance problem and not merely a missed cron. " +
				"IT IS SEEDED TO 0 AT STARTUP, NOT LEFT ABSENT — a bare " +
				"'now() - gauge > 48h' rule would silently never fire against an absent " +
				"series, so the sweep publishes 0 at registration and the alert reads as " +
				"firing from the moment the process starts until the first pass succeeds. " +
				"That is deliberate: a process that never reaches 03:00 UTC is exactly " +
				"the failure this gauge exists to catch. Suppress the startup window with " +
				"a 'for' clause longer than one day rather than by ignoring zero.",
		}),
	}
	reg.MustRegister(m.drivesDeleted, m.batches, m.batchErrors, m.runDuration, m.lastSuccess)
	// Publish the freshness gauge immediately rather than leaving the series
	// absent until the first 03:00 UTC pass. A plain Gauge already exports 0 on
	// registration, so this Set is redundant TODAY — it is here to state the
	// requirement, because the staleness alert is only sound if the series
	// exists from startup, and a future switch to a GaugeVec (which publishes
	// nothing until a child is touched) would break that silently.
	m.lastSuccess.Set(0)
	return m
}

func (m *PrometheusMetrics) AddDrivesDeleted(n int) {
	if n > 0 {
		m.drivesDeleted.Add(float64(n))
	}
}

func (m *PrometheusMetrics) IncBatch() { m.batches.Inc() }

func (m *PrometheusMetrics) IncBatchError() { m.batchErrors.Inc() }

func (m *PrometheusMetrics) ObserveRunDuration(d time.Duration) {
	m.runDuration.Observe(d.Seconds())
}

func (m *PrometheusMetrics) SetLastSuccess(t time.Time) {
	m.lastSuccess.Set(float64(t.Unix()))
}
