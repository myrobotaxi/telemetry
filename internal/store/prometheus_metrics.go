package store

import "github.com/prometheus/client_golang/prometheus"

// queryDurationBuckets extends prometheus.DefBuckets with low-end
// resolution so sub-100ms queries (the common case for pgx point
// lookups) land in distinct buckets instead of collapsing into the
// first DefBuckets bucket (5ms). MYR-132 found the testbench was
// flying blind on the [1ms, 50ms] band where DB query histograms are
// most informative.
var queryDurationBuckets = []float64{
	0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
}

// PrometheusMetrics implements Metrics against a Prometheus registry.
// Register it once at startup via NewPrometheusMetrics and share the
// returned pointer across all store constructors (NewDB,
// NewVehicleRepoWithEncryption, NewDriveRepoWithEncryption) so query
// timings and the connection pool gauges land on a single coherent
// observability surface under the telemetry_store_* prefix.
type PrometheusMetrics struct {
	queryDuration   *prometheus.HistogramVec
	queryErrors     *prometheus.CounterVec
	decryptFailures *prometheus.CounterVec
	poolAcquired    prometheus.Gauge
	poolIdle        prometheus.Gauge
	poolTotal       prometheus.Gauge
}

var _ Metrics = (*PrometheusMetrics)(nil)

// NewPrometheusMetrics registers the five store metric series on reg
// and returns a Metrics implementation ready to pass into the store
// constructors. Mirrors the auditsidecar.NewPrometheusMetrics pattern:
// one constructor, MustRegister at startup so a duplicate registration
// is a fail-fast configuration error rather than a silent miss.
//
// Operation labels follow the "entity.method" convention from
// store/metrics.go (e.g., "vehicle.get_by_id", "drive.create"). These
// strings are P2 (operational metadata) — they must never carry P1
// fields like VINs or userIds.
func NewPrometheusMetrics(reg prometheus.Registerer) *PrometheusMetrics {
	queryDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "telemetry_store_query_duration_seconds",
		Help:    "Latency of database operations in seconds, labelled by entity.method. Buckets are tuned for the pgx sub-100ms point-lookup regime that DefBuckets collapses into a single 5ms bucket.",
		Buckets: queryDurationBuckets,
	}, []string{"operation"})
	reg.MustRegister(queryDuration)

	queryErrors := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "telemetry_store_query_errors_total",
		Help: "Count of database operations that returned an error, labelled by entity.method. Alert on rate > 0 for sustained windows; spikes commonly indicate pool exhaustion or a Supabase connection rotation.",
	}, []string{"operation"})
	reg.MustRegister(queryErrors)

	decryptFailures := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "telemetry_store_decrypt_failures_total",
		Help: "Count of at-rest ciphertext columns that failed to decrypt on a read, labelled by column. Since MYR-433 these reads are fail-soft — a column that will not decrypt surfaces as absent rather than an error — so this counter is the ONLY signal that a wrong ENCRYPTION_KEY or a corrupt row is silently blanking user data. Alert on rate > 0.",
	}, []string{"column"})
	reg.MustRegister(decryptFailures)

	poolAcquired := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "telemetry_store_pool_acquired_conns",
		Help: "Connections currently checked out of the pgxpool by in-flight queries. Sustained values at MaxConns indicate the pool is the bottleneck and queries are queuing.",
	})
	reg.MustRegister(poolAcquired)

	poolIdle := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "telemetry_store_pool_idle_conns",
		Help: "Connections currently idle in the pgxpool, available for immediate acquisition. A persistent idle=0 with acquired=MaxConns means contention.",
	})
	reg.MustRegister(poolIdle)

	poolTotal := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "telemetry_store_pool_total_conns",
		Help: "Total connections (acquired + idle) the pgxpool currently holds open, bounded by MaxConns. Useful for confirming the pool grew under load and for sizing MaxConns against Supabase's connection limit.",
	})
	reg.MustRegister(poolTotal)

	return &PrometheusMetrics{
		queryDuration:   queryDuration,
		queryErrors:     queryErrors,
		decryptFailures: decryptFailures,
		poolAcquired:    poolAcquired,
		poolIdle:        poolIdle,
		poolTotal:       poolTotal,
	}
}

// ObserveQueryDuration records a query latency sample under the given
// operation label.
func (m *PrometheusMetrics) ObserveQueryDuration(operation string, seconds float64) {
	m.queryDuration.WithLabelValues(operation).Observe(seconds)
}

// IncQueryError increments the error counter for the given operation.
func (m *PrometheusMetrics) IncQueryError(operation string) {
	m.queryErrors.WithLabelValues(operation).Inc()
}

// IncDecryptFailure increments the decrypt-failure counter for the given
// at-rest column.
func (m *PrometheusMetrics) IncDecryptFailure(column string) {
	m.decryptFailures.WithLabelValues(column).Inc()
}

// SetPoolStats updates the three connection-pool gauges. Called from
// the periodic collector goroutine in cmd/telemetry-server that polls
// DB.CollectPoolStats every 15s.
func (m *PrometheusMetrics) SetPoolStats(acquired, idle, total int32) {
	m.poolAcquired.Set(float64(acquired))
	m.poolIdle.Set(float64(idle))
	m.poolTotal.Set(float64(total))
}
