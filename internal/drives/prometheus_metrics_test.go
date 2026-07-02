package drives

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// findDriveMetricFamily returns the named family from a Gather result,
// or nil when absent.
func findDriveMetricFamily(mfs []*dto.MetricFamily, name string) *dto.MetricFamily {
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf
		}
	}
	return nil
}

// TestPrometheusDetectorMetrics_RegistersAndCounts exercises every
// DetectorMetrics method against a fresh registry and asserts each
// series lands under the expected name with the expected value —
// including the MYR-160 stall / duration-cap end-reason counters that
// production now wires in place of NoopDetectorMetrics.
func TestPrometheusDetectorMetrics_RegistersAndCounts(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusDetectorMetrics(reg)

	m.IncDriveStarted()
	m.IncDriveEnded()
	m.IncMicroDriveDiscarded()
	m.IncDebounceCancelled()
	m.IncWatchdogEnded()
	m.IncStallEnded()
	m.IncStallEnded()
	m.IncDurationCapEnded()
	m.ObserveDriveDuration(1234)
	m.ObserveDriveDistance(5.6)
	m.SetActiveVehicles(3)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	counterWant := map[string]float64{
		"telemetry_drives_started_total":            1,
		"telemetry_drives_ended_total":              1,
		"telemetry_drives_micro_discarded_total":    1,
		"telemetry_drives_debounce_cancelled_total": 1,
		"telemetry_drives_watchdog_ended_total":     1,
		"telemetry_drives_stall_ended_total":        2,
		"telemetry_drives_duration_cap_ended_total": 1,
	}
	for name, want := range counterWant {
		t.Run(name, func(t *testing.T) {
			mf := findDriveMetricFamily(mfs, name)
			if mf == nil {
				t.Fatalf("metric family %q not registered", name)
			}
			if got := mf.GetMetric()[0].GetCounter().GetValue(); got != want {
				t.Errorf("%s = %v, want %v", name, got, want)
			}
		})
	}

	t.Run("telemetry_drives_active_vehicles", func(t *testing.T) {
		mf := findDriveMetricFamily(mfs, "telemetry_drives_active_vehicles")
		if mf == nil {
			t.Fatal("gauge not registered")
		}
		if got := mf.GetMetric()[0].GetGauge().GetValue(); got != 3 {
			t.Errorf("active vehicles gauge = %v, want 3", got)
		}
	})

	histWant := map[string]float64{
		"telemetry_drives_duration_seconds": 1234,
		"telemetry_drives_distance_miles":   5.6,
	}
	for name, wantSum := range histWant {
		t.Run(name, func(t *testing.T) {
			mf := findDriveMetricFamily(mfs, name)
			if mf == nil {
				t.Fatalf("histogram %q not registered", name)
			}
			h := mf.GetMetric()[0].GetHistogram()
			if h.GetSampleCount() != 1 {
				t.Errorf("sample count = %d, want 1", h.GetSampleCount())
			}
			if h.GetSampleSum() != wantSum {
				t.Errorf("sample sum = %v, want %v", h.GetSampleSum(), wantSum)
			}
		})
	}
}

// TestPrometheusDetectorMetrics_DuplicateRegistrationPanics pins the
// fail-fast contract: registering the series twice on one registry is
// a configuration error surfaced at startup, not a silent miss.
func TestPrometheusDetectorMetrics_DuplicateRegistrationPanics(t *testing.T) {
	reg := prometheus.NewRegistry()
	_ = NewPrometheusDetectorMetrics(reg)

	defer func() {
		if recover() == nil {
			t.Error("expected panic on duplicate registration")
		}
	}()
	_ = NewPrometheusDetectorMetrics(reg)
}
