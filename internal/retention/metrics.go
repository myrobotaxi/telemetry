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
				"Expect a large first-run spike against the initial backlog, then a " +
				"low daily rate. A flat line while telemetry_pruner_batches_processed_total " +
				"also stays flat means the sweep is not running.",
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
				"(nothing to do) to tens of minutes (first-run backlog).",
			Buckets: []float64{0.01, 0.1, 1, 10, 60, 300, 900, 3600},
		}),
		lastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "telemetry_pruner_last_success_timestamp_seconds",
			Help: "Unix timestamp of the last retention pass that completed without a " +
				"batch error. Alert when now() minus this exceeds ~48h: the retention " +
				"promise is a privacy commitment, so a silently stalled sweep is a " +
				"compliance problem and not merely a missed cron.",
		}),
	}
	reg.MustRegister(m.drivesDeleted, m.batches, m.batchErrors, m.runDuration, m.lastSuccess)
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
