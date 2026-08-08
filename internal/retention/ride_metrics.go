package retention

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Ride retention sweep metrics (MYR-447, data-lifecycle.md §5.7).
//
// EVERY NAME HERE IS telemetry_ride_pruner_*, distinct from the drive sweep's
// telemetry_pruner_*. Not a style choice: both collector sets are registered
// into the same registry by the same process, and a duplicate metric name is a
// panic at MustRegister, i.e. a crash loop on boot rather than a bad dashboard.

// PrometheusRideMetrics is the live collector set for the ride retention sweep.
type PrometheusRideMetrics struct {
	ridesDeleted       prometheus.Counter
	passengersScrubbed prometheus.Counter
	batches            prometheus.Counter
	batchErrors        prometheus.Counter
	runDuration        prometheus.Histogram
	lastSuccess        prometheus.Gauge
}

// NewPrometheusRideMetrics builds and registers the ride sweep's collectors.
func NewPrometheusRideMetrics(reg prometheus.Registerer) *PrometheusRideMetrics {
	m := &PrometheusRideMetrics{
		ridesDeleted: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "telemetry_ride_pruner_rides_deleted_total",
			Help: "Terminal go_ride_requests rows (completed / declined / cancelled) " +
				"deleted by the 365-day retention sweep, cumulative. EXPECT ZERO FOR " +
				"THE FIRST YEAR OF THE TABLE'S LIFE: eligibility is keyed off " +
				"updated_at, so no row can qualify until 365 days after it went " +
				"terminal, and a flat line before then is the correct reading rather " +
				"than a stalled sweep. Use " +
				"telemetry_ride_pruner_last_success_timestamp_seconds to tell " +
				"'nothing to do' from 'not running'. OPEN rides are never counted " +
				"here — they are not in the sweep's claim at all.",
		}),
		passengersScrubbed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "telemetry_ride_pruner_passengers_scrubbed_total",
			Help: "Surviving terminal rides whose deprecated passenger_name and " +
				"passenger_phone columns were NULLed at terminal + 30 days, " +
				"cumulative. The book-for-someone-else feature was removed in " +
				"MYR-382, so in steady state this should sit at zero: a sustained " +
				"non-zero rate means something is still WRITING those columns and " +
				"wants finding, not that the scrub is working harder.",
		}),
		batches: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "telemetry_ride_pruner_batches_processed_total",
			Help: "Ride retention batches committed, cumulative. Divided by " +
				"rides_deleted this gives the average batch fill; a ratio far below " +
				"the batch size means the sweep is keeping up.",
		}),
		batchErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "telemetry_ride_pruner_batch_errors_total",
			Help: "Ride retention batches that failed every attempt and ended their " +
				"pass. Alert on any increase: the failed batch is retried next run, " +
				"but a persistent error means ride records are aging past the " +
				"promised window.",
		}),
		runDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "telemetry_ride_pruner_run_duration_seconds",
			Help: "Wall-clock duration of one ride retention pass. Buckets span " +
				"sub-second (nothing to do — the expected case) to tens of minutes " +
				"(a large backlog, should one ever accumulate).",
			Buckets: []float64{0.01, 0.1, 1, 10, 60, 300, 900, 3600},
		}),
		lastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "telemetry_ride_pruner_last_success_timestamp_seconds",
			Help: "Unix timestamp of the last ride retention pass that completed " +
				"without a batch error. Alert when now() minus this exceeds ~48h: " +
				"the retention promise is a privacy commitment, so a silently " +
				"stalled sweep is a compliance problem and not merely a missed cron. " +
				"IT IS SEEDED TO 0 AT STARTUP, NOT LEFT ABSENT — a bare " +
				"'now() - gauge > 48h' rule would silently never fire against an " +
				"absent series, so the sweep publishes 0 at registration and the " +
				"alert reads as firing from the moment the process starts until the " +
				"first pass succeeds. Suppress the startup window with a 'for' " +
				"clause longer than one day rather than by ignoring zero.",
		}),
	}
	reg.MustRegister(m.ridesDeleted, m.passengersScrubbed, m.batches, m.batchErrors,
		m.runDuration, m.lastSuccess)
	// Publish the freshness gauge immediately rather than leaving the series
	// absent until the first 03:00 UTC pass — same reasoning as the drive
	// sweep's gauge, and the same requirement on any future switch to a
	// GaugeVec, which publishes nothing until a child is touched.
	m.lastSuccess.Set(0)
	return m
}

func (m *PrometheusRideMetrics) AddRidesDeleted(n int) {
	if n > 0 {
		m.ridesDeleted.Add(float64(n))
	}
}

func (m *PrometheusRideMetrics) AddPassengersScrubbed(n int) {
	if n > 0 {
		m.passengersScrubbed.Add(float64(n))
	}
}

func (m *PrometheusRideMetrics) IncBatch() { m.batches.Inc() }

func (m *PrometheusRideMetrics) IncBatchError() { m.batchErrors.Inc() }

func (m *PrometheusRideMetrics) ObserveRunDuration(d time.Duration) {
	m.runDuration.Observe(d.Seconds())
}

func (m *PrometheusRideMetrics) SetLastSuccess(t time.Time) {
	m.lastSuccess.Set(float64(t.Unix()))
}
