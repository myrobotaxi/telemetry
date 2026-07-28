package telemetry

import (
	"errors"

	"github.com/prometheus/client_golang/prometheus"
)

// ReceiverMetrics collects telemetry receiver operational metrics.
// Implementations must be safe for concurrent use.
type ReceiverMetrics interface {
	// IncMessagesReceived increments the count of messages received
	// from a vehicle identified by its redacted VIN.
	IncMessagesReceived(vin string)

	// IncDecodeErrors increments the count of protobuf decode errors
	// for a vehicle.
	IncDecodeErrors(vin string)

	// IncRateLimited increments the count of messages dropped due to
	// per-vehicle rate limiting.
	IncRateLimited(vin string)

	// SetConnectedVehicles sets the current number of connected vehicles.
	SetConnectedVehicles(count int)

	// ObserveMessageLatency records the time taken to process a single
	// message from receipt through event bus publication.
	ObserveMessageLatency(seconds float64)

	// IncFieldDecodeError increments the count of per-field decode errors.
	// The vin should be redacted and field is the internal field name.
	IncFieldDecodeError(vin, field string)

	// IncRejectedVINNotAuthorized increments the count of inbound frames
	// rejected because the VIN has no matching Vehicle row (data
	// lifecycle §3.5 cleanup, MYR-73). Backs the Prometheus counter
	// `tesla_inbound_rejected_total{reason="vehicle_not_authorized"}`.
	// Per-VIN drilldown lives in slog.Warn at the call site (high
	// cardinality precludes a VIN label here).
	IncRejectedVINNotAuthorized()
}

// NoopReceiverMetrics is a ReceiverMetrics where all methods are no-ops.
// Use it in tests or when metrics collection is not required.
type NoopReceiverMetrics struct{}

var _ ReceiverMetrics = NoopReceiverMetrics{}

func (NoopReceiverMetrics) IncMessagesReceived(string)      {}
func (NoopReceiverMetrics) IncDecodeErrors(string)          {}
func (NoopReceiverMetrics) IncRateLimited(string)           {}
func (NoopReceiverMetrics) SetConnectedVehicles(int)        {}
func (NoopReceiverMetrics) ObserveMessageLatency(float64)   {}
func (NoopReceiverMetrics) IncFieldDecodeError(string, string) {}
func (NoopReceiverMetrics) IncRejectedVINNotAuthorized() {}

// PrometheusReceiverMetrics implements ReceiverMetrics using Prometheus.
type PrometheusReceiverMetrics struct {
	messagesReceived  *prometheus.CounterVec
	decodeErrors      *prometheus.CounterVec
	rateLimited       *prometheus.CounterVec
	fieldDecodeErrors          *prometheus.CounterVec
	inboundRejectedNotAuthorized *prometheus.CounterVec
	connectedVehicles prometheus.Gauge
	messageLatency    prometheus.Histogram
}

var _ ReceiverMetrics = (*PrometheusReceiverMetrics)(nil)

// NewPrometheusReceiverMetrics creates and registers Prometheus metrics
// for the telemetry receiver.
func NewPrometheusReceiverMetrics(reg prometheus.Registerer) *PrometheusReceiverMetrics {
	m := &PrometheusReceiverMetrics{
		messagesReceived: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "telemetry",
			Subsystem: "receiver",
			Name:      "messages_received_total",
			Help:      "Total messages received from vehicles.",
		}, []string{"vin"}),

		decodeErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "telemetry",
			Subsystem: "receiver",
			Name:      "decode_errors_total",
			Help:      "Total protobuf decode errors per vehicle.",
		}, []string{"vin"}),

		rateLimited: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "telemetry",
			Subsystem: "receiver",
			Name:      "rate_limited_total",
			Help:      "Total messages dropped due to rate limiting per vehicle.",
		}, []string{"vin"}),

		fieldDecodeErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "telemetry",
			Subsystem: "receiver",
			Name:      "field_decode_errors_total",
			Help:      "Total per-field decode errors by vehicle and field name.",
		}, []string{"vin", "field"}),

		inboundRejectedNotAuthorized: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "tesla_inbound_rejected_total",
			Help: "Total inbound mTLS frames rejected, by reason. Reason vehicle_not_authorized fires when the VIN has no matching Vehicle row (data-lifecycle §3.5).",
		}, []string{"reason"}),

		connectedVehicles: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: "telemetry",
			Subsystem: "receiver",
			Name:      "connected_vehicles",
			Help:      "Current number of connected vehicles.",
		}),

		messageLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Namespace: "telemetry",
			Subsystem: "receiver",
			Name:      "message_latency_seconds",
			Help:      "Time to process a message from receipt to event bus publish.",
			Buckets:   []float64{0.0001, 0.0005, 0.001, 0.005, 0.01, 0.05, 0.1},
		}),
	}

	collectors := []prometheus.Collector{
		m.messagesReceived,
		m.decodeErrors,
		m.rateLimited,
		m.fieldDecodeErrors,
		m.inboundRejectedNotAuthorized,
		m.connectedVehicles,
		m.messageLatency,
	}
	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			// If already registered (e.g., in tests), use the existing collector.
			var are prometheus.AlreadyRegisteredError
			if errors.As(err, &are) {
				continue
			}
			panic(err) // unexpected registration error
		}
	}
	return m
}

func (m *PrometheusReceiverMetrics) IncMessagesReceived(vin string) {
	m.messagesReceived.WithLabelValues(vin).Inc()
}

func (m *PrometheusReceiverMetrics) IncDecodeErrors(vin string) {
	m.decodeErrors.WithLabelValues(vin).Inc()
}

func (m *PrometheusReceiverMetrics) IncRateLimited(vin string) {
	m.rateLimited.WithLabelValues(vin).Inc()
}

func (m *PrometheusReceiverMetrics) SetConnectedVehicles(count int) {
	m.connectedVehicles.Set(float64(count))
}

func (m *PrometheusReceiverMetrics) ObserveMessageLatency(seconds float64) {
	m.messageLatency.Observe(seconds)
}

func (m *PrometheusReceiverMetrics) IncFieldDecodeError(vin, field string) {
	m.fieldDecodeErrors.WithLabelValues(vin, field).Inc()
}

// IncRejectedVINNotAuthorized increments the rejection counter with
// a constant reason label. Per-VIN drilldown lives in slog.Warn at
// the call site (high cardinality precludes a VIN label here).
func (m *PrometheusReceiverMetrics) IncRejectedVINNotAuthorized() {
	m.inboundRejectedNotAuthorized.WithLabelValues("vehicle_not_authorized").Inc()
}
