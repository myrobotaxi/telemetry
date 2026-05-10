package auditsidecar

import "github.com/prometheus/client_golang/prometheus"

// Metrics is the observability contract for the sidecar. Implementations
// must be safe for concurrent use.
//
// All three counters are pre-registered at startup so they appear in /metrics
// on the first scrape with value 0 even before any AuditLog entry is written.
// This is the same pattern used by cryptox.Metrics and store.Metrics.
type Metrics interface {
	// IncWrite records one successful PutObject delivered to S3.
	IncWrite()
	// IncFailure records one entry that was permanently dropped after
	// exhausting all retries. reason is one of "put", "enqueue_full",
	// "other".
	IncFailure(reason string)
	// SetQueueDepth records the current length of the in-process queue.
	SetQueueDepth(n int)
}

// NoopMetrics is the zero-cost default for tests and local dev.
type NoopMetrics struct{}

func (NoopMetrics) IncWrite()             {}
func (NoopMetrics) IncFailure(_ string)   {}
func (NoopMetrics) SetQueueDepth(_ int)   {}

// PrometheusMetrics implements Metrics against a Prometheus registry.
// Register it at startup via NewPrometheusMetrics and pass it to
// NewS3Sidecar.
type PrometheusMetrics struct {
	writes   prometheus.Counter
	failures *prometheus.CounterVec
	depth    prometheus.Gauge
}

// failureReasons enumerates the label values pre-registered at startup so
// /metrics always shows all series even before the first failure.
var failureReasons = []string{"put", "enqueue_full", "other"}

// NewPrometheusMetrics registers the three sidecar metric series on reg and
// pre-initialises every label so operators see zero-valued counters on the
// first scrape rather than missing series. Call this once from the
// composition root (cmd/telemetry-server/wiring.go).
func NewPrometheusMetrics(reg prometheus.Registerer) *PrometheusMetrics {
	writes := prometheus.NewCounter(prometheus.CounterOpts{
		Name: "audit_sidecar_writes_total",
		Help: "Total AuditLog entries successfully written to the S3 sidecar bucket.",
	})
	reg.MustRegister(writes)

	failures := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "audit_sidecar_write_failures_total",
		Help: "Total AuditLog sidecar write failures by reason. reason=put means the S3 PutObject (against AWS S3 or any S3-compatible backend, e.g. Supabase Storage) failed after all retries. reason=enqueue_full means the internal channel was at capacity. reason=other means an unexpected error.",
	}, []string{"reason"})
	reg.MustRegister(failures)
	// Pre-register all label values.
	for _, r := range failureReasons {
		failures.WithLabelValues(r).Add(0)
	}

	depth := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "audit_sidecar_queue_depth",
		Help: "Current number of AuditLog entries waiting in the in-process sidecar queue. Alert if persistently near the queue capacity (10 000).",
	})
	reg.MustRegister(depth)

	return &PrometheusMetrics{
		writes:   writes,
		failures: failures,
		depth:    depth,
	}
}

// IncWrite increments the writes counter.
func (m *PrometheusMetrics) IncWrite() { m.writes.Inc() }

// IncFailure increments the labeled failure counter. Unknown reason values
// are normalised to "other" to avoid unbounded label cardinality.
func (m *PrometheusMetrics) IncFailure(reason string) {
	// Guard against unknown labels.
	valid := false
	for _, r := range failureReasons {
		if r == reason {
			valid = true
			break
		}
	}
	if !valid {
		reason = "other"
	}
	m.failures.WithLabelValues(reason).Inc()
}

// SetQueueDepth updates the depth gauge.
func (m *PrometheusMetrics) SetQueueDepth(n int) { m.depth.Set(float64(n)) }
