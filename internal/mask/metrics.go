package mask

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// PrometheusAuditMetrics implements AuditMetrics using Prometheus
// counters. The label cardinality is bounded by the closed enums
// (action, targetType) defined in data-lifecycle.md §4.2, which keeps
// Prometheus storage footprint tiny.
type PrometheusAuditMetrics struct {
	writes   *prometheus.CounterVec
	failures *prometheus.CounterVec
}

var _ AuditMetrics = (*PrometheusAuditMetrics)(nil)

// NewPrometheusAuditMetrics creates and registers the audit-log write
// counters. Re-registration of an already-registered collector is
// tolerated so the same metrics can survive multiple New... calls in
// tests.
func NewPrometheusAuditMetrics(reg prometheus.Registerer) *PrometheusAuditMetrics {
	m := &PrometheusAuditMetrics{
		writes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "telemetry",
			Subsystem: "audit",
			Name:      "log_writes_total",
			Help:      "Total successful AuditLog inserts, labeled by action and target.",
		}, []string{"action", "target"}),

		failures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "telemetry",
			Subsystem: "audit",
			Name:      "log_write_failures_total",
			Help:      "Total failed AuditLog inserts, labeled by action and target.",
		}, []string{"action", "target"}),
	}

	for _, c := range []prometheus.Collector{m.writes, m.failures} {
		if err := reg.Register(c); err != nil {
			var are prometheus.AlreadyRegisteredError
			if errors.As(err, &are) {
				continue
			}
			panic(err) // unexpected registration error
		}
	}
	return m
}

// IncAuditWrite implements AuditMetrics.
func (m *PrometheusAuditMetrics) IncAuditWrite(action, target string) {
	m.writes.WithLabelValues(action, target).Inc()
}

// IncAuditWriteFailure implements AuditMetrics.
func (m *PrometheusAuditMetrics) IncAuditWriteFailure(action, target string) {
	m.failures.WithLabelValues(action, target).Inc()
}
