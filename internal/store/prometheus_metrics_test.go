package store

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// TestPrometheusMetrics_RegistersAllSeries asserts the constructor
// publishes every store metric family on the registry so a /metrics
// scrape immediately after startup surfaces the full surface (even
// before the first query). Mirrors the cryptox pattern: zero series is
// a regression, missing names are a regression, and a duplicate
// registration would fail-fast inside MustRegister.
func TestPrometheusMetrics_RegistersAllSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics(reg)

	// HistogramVec and CounterVec children are materialized lazily —
	// Gather() returns no metric family for them until at least one
	// WithLabelValues observation. Force one for each so the
	// registration assertion below covers all 5 series, not just the 3
	// label-free Gauges. Observe(0) and Add(0) record without skewing
	// any subsequent test that uses its own registry.
	m.ObserveQueryDuration("_startup", 0)
	m.queryErrors.WithLabelValues("_startup").Add(0)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	want := []string{
		"telemetry_store_query_duration_seconds",
		"telemetry_store_query_errors_total",
		"telemetry_store_pool_acquired_conns",
		"telemetry_store_pool_idle_conns",
		"telemetry_store_pool_total_conns",
	}
	for _, name := range want {
		if findMetricFamily(mfs, name) == nil {
			t.Errorf("metric family %q not registered", name)
		}
	}
}

// TestPrometheusMetrics_ObserveQueryDurationRecordsSample asserts a
// single Observe call lands in the histogram under the matching
// operation label.
func TestPrometheusMetrics_ObserveQueryDurationRecordsSample(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics(reg)

	m.ObserveQueryDuration("vehicle.get_by_id", 0.123)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	mf := findMetricFamily(mfs, "telemetry_store_query_duration_seconds")
	if mf == nil {
		t.Fatal("telemetry_store_query_duration_seconds not registered")
	}

	var sample *dto.Histogram
	for _, mt := range mf.Metric {
		if labelValue(mt, "operation") == "vehicle.get_by_id" {
			sample = mt.GetHistogram()
			break
		}
	}
	if sample == nil {
		t.Fatal("no histogram sample for operation=vehicle.get_by_id")
	}
	if got, want := sample.GetSampleCount(), uint64(1); got != want {
		t.Errorf("sample count = %d, want %d", got, want)
	}
	if got, want := sample.GetSampleSum(), 0.123; got != want {
		t.Errorf("sample sum = %v, want %v", got, want)
	}
}

// TestPrometheusMetrics_IncQueryErrorIncrementsCounter asserts the
// labelled error counter increments and the unrelated operation label
// stays at zero (no cross-label leakage).
func TestPrometheusMetrics_IncQueryErrorIncrementsCounter(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics(reg)

	m.IncQueryError("drive.create")
	m.IncQueryError("drive.create")

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	mf := findMetricFamily(mfs, "telemetry_store_query_errors_total")
	if mf == nil {
		t.Fatal("telemetry_store_query_errors_total not registered")
	}

	got := map[string]float64{}
	for _, mt := range mf.Metric {
		got[labelValue(mt, "operation")] = mt.GetCounter().GetValue()
	}
	if got["drive.create"] != 2 {
		t.Errorf("drive.create counter = %v, want 2", got["drive.create"])
	}
	if v, ok := got["vehicle.get_by_id"]; ok && v != 0 {
		t.Errorf("vehicle.get_by_id counter = %v, want 0 (no cross-label leakage)", v)
	}
}

// TestPrometheusMetrics_SetPoolStatsReflectsValues asserts the three
// pool gauges reflect the most recent SetPoolStats call. This is the
// end-to-end check that the periodic collector goroutine in
// cmd/telemetry-server flows DB.CollectPoolStats() into /metrics.
func TestPrometheusMetrics_SetPoolStatsReflectsValues(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics(reg)

	m.SetPoolStats(5, 2, 7)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	tests := []struct {
		name string
		want float64
	}{
		{"telemetry_store_pool_acquired_conns", 5},
		{"telemetry_store_pool_idle_conns", 2},
		{"telemetry_store_pool_total_conns", 7},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mf := findMetricFamily(mfs, tt.name)
			if mf == nil {
				t.Fatalf("%s not registered", tt.name)
			}
			if got := len(mf.Metric); got != 1 {
				t.Fatalf("metric count = %d, want 1", got)
			}
			if got := mf.Metric[0].GetGauge().GetValue(); got != tt.want {
				t.Errorf("%s = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// findMetricFamily returns the *dto.MetricFamily with the given name,
// or nil if absent.
func findMetricFamily(mfs []*dto.MetricFamily, name string) *dto.MetricFamily {
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf
		}
	}
	return nil
}

// labelValue returns the value for a given label name on a Metric, or
// "" if the label is absent.
func labelValue(m *dto.Metric, name string) string {
	for _, l := range m.Label {
		if l.GetName() == name {
			return l.GetValue()
		}
	}
	return ""
}
