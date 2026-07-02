package drives

import "github.com/prometheus/client_golang/prometheus"

// driveDurationBuckets covers the realistic drive-length range: a few
// minutes up to the 12h MaxDriveDuration backstop, in seconds.
var driveDurationBuckets = []float64{
	60, 300, 600, 1200, 1800, 2700, 3600, 7200, 14400, 28800, 43200,
}

// driveDistanceBuckets covers per-drive miles from micro-drive scale
// up to long road-trip legs.
var driveDistanceBuckets = []float64{
	0.1, 0.5, 1, 2, 5, 10, 25, 50, 100, 200, 400,
}

// PrometheusDetectorMetrics implements DetectorMetrics against a
// Prometheus registry. Register once at startup via
// NewPrometheusDetectorMetrics and pass into NewDetector. The
// per-end-reason counters (watchdog silence / stall / duration cap,
// MYR-160) are the observable record of why drives closed — without
// them a stuck-drive regression is invisible until users notice
// fragmented history.
type PrometheusDetectorMetrics struct {
	driveStarted       prometheus.Counter
	driveEnded         prometheus.Counter
	microDiscarded     prometheus.Counter
	debounceCancelled  prometheus.Counter
	watchdogEnded      prometheus.Counter
	stallEnded         prometheus.Counter
	durationCapEnded   prometheus.Counter
	driveDurationHist  prometheus.Histogram
	driveDistanceHist  prometheus.Histogram
	activeVehicleGauge prometheus.Gauge
}

var _ DetectorMetrics = (*PrometheusDetectorMetrics)(nil)

// NewPrometheusDetectorMetrics registers the drive-detector metric
// series on reg and returns a DetectorMetrics ready to pass into
// NewDetector. Mirrors store.NewPrometheusMetrics: MustRegister at
// startup so a duplicate registration fails fast. All series are P2
// operational metadata — no VINs or user identifiers.
func NewPrometheusDetectorMetrics(reg prometheus.Registerer) *PrometheusDetectorMetrics {
	m := &PrometheusDetectorMetrics{
		driveStarted: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "telemetry_drives_started_total",
			Help: "Count of drives started by the detector (gear shifted to D/R).",
		}),
		driveEnded: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "telemetry_drives_ended_total",
			Help: "Count of drives ended and published (passed the micro-drive filter).",
		}),
		microDiscarded: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "telemetry_drives_micro_discarded_total",
			Help: "Count of drives discarded by the micro-drive filter (below MinDuration/MinDistanceMiles); their DB rows are deleted via drive.discarded.",
		}),
		debounceCancelled: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "telemetry_drives_debounce_cancelled_total",
			Help: "Count of gear=P end debounces cancelled because the vehicle resumed driving.",
		}),
		watchdogEnded: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "telemetry_drives_watchdog_ended_total",
			Help: "Count of drives ended because telemetry went silent for EndDebounce (MYR-139 R3a).",
		}),
		stallEnded: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "telemetry_drives_stall_ended_total",
			Help: "Count of drives ended because telemetry kept flowing with no movement for StallTimeout — the missed gear=P case (MYR-160).",
		}),
		durationCapEnded: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "telemetry_drives_duration_cap_ended_total",
			Help: "Count of drives force-ended by the MaxDriveDuration backstop (MYR-160). Non-zero rates warrant investigation.",
		}),
		driveDurationHist: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "telemetry_drives_duration_seconds",
			Help:    "Distribution of completed drive durations in seconds.",
			Buckets: driveDurationBuckets,
		}),
		driveDistanceHist: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "telemetry_drives_distance_miles",
			Help:    "Distribution of completed drive distances in miles (odometer delta when available, GPS fallback).",
			Buckets: driveDistanceBuckets,
		}),
		activeVehicleGauge: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "telemetry_drives_active_vehicles",
			Help: "Vehicles currently in the Driving state.",
		}),
	}

	reg.MustRegister(
		m.driveStarted, m.driveEnded, m.microDiscarded,
		m.debounceCancelled, m.watchdogEnded, m.stallEnded,
		m.durationCapEnded, m.driveDurationHist, m.driveDistanceHist,
		m.activeVehicleGauge,
	)
	return m
}

func (m *PrometheusDetectorMetrics) IncDriveStarted()        { m.driveStarted.Inc() }
func (m *PrometheusDetectorMetrics) IncDriveEnded()          { m.driveEnded.Inc() }
func (m *PrometheusDetectorMetrics) IncMicroDriveDiscarded() { m.microDiscarded.Inc() }
func (m *PrometheusDetectorMetrics) IncDebounceCancelled()   { m.debounceCancelled.Inc() }
func (m *PrometheusDetectorMetrics) IncWatchdogEnded()       { m.watchdogEnded.Inc() }
func (m *PrometheusDetectorMetrics) IncStallEnded()          { m.stallEnded.Inc() }
func (m *PrometheusDetectorMetrics) IncDurationCapEnded()    { m.durationCapEnded.Inc() }

func (m *PrometheusDetectorMetrics) ObserveDriveDuration(seconds float64) {
	m.driveDurationHist.Observe(seconds)
}

func (m *PrometheusDetectorMetrics) ObserveDriveDistance(miles float64) {
	m.driveDistanceHist.Observe(miles)
}

func (m *PrometheusDetectorMetrics) SetActiveVehicles(count int) {
	m.activeVehicleGauge.Set(float64(count))
}
