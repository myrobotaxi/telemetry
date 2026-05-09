package accountbackfill_test

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/tnando/my-robo-taxi-telemetry/internal/store/accountbackfill"
)

// TestPlaintextGauge_NameAndLabels asserts the gauge metric name and
// per-column labels are exposed on the registry without needing a
// database. This is the unit-level guard for acceptance criterion 4
// ("metric registered + visible on /metrics").
func TestPlaintextGauge_NameAndLabels(t *testing.T) {
	reg := prometheus.NewRegistry()

	// pool=nil is safe because we never call Run; we only exercise Set.
	gauge := accountbackfill.NewPlaintextGauge(reg, nil, time.Hour, nil)

	gauge.Set(map[string]int{"access_token": 7, "refresh_token": 3, "id_token": 0})

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	got := readGaugeFromFamilies(mfs, "account_token_plaintext_remaining_total")
	if got == nil {
		t.Fatal("metric account_token_plaintext_remaining_total not registered")
	}

	wantPerColumn := map[string]float64{
		"access_token":  7,
		"refresh_token": 3,
		"id_token":      0,
	}
	if len(got) != len(wantPerColumn) {
		t.Errorf("got %d label series, want %d (%v)", len(got), len(wantPerColumn), got)
	}
	for col, want := range wantPerColumn {
		if got[col] != want {
			t.Errorf("column=%s: got %v, want %v", col, got[col], want)
		}
	}
}

// TestPlaintextGauge_PreRegistersAllColumns verifies the constructor
// pre-seeds zero values for every TokenColumn so a `/metrics` scrape
// before the first refresh still surfaces three labelled samples
// instead of an empty metric family.
func TestPlaintextGauge_PreRegistersAllColumns(t *testing.T) {
	reg := prometheus.NewRegistry()
	_ = accountbackfill.NewPlaintextGauge(reg, nil, time.Hour, nil)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	got := readGaugeFromFamilies(mfs, "account_token_plaintext_remaining_total")
	for _, col := range accountbackfill.TokenColumns {
		v, ok := got[col]
		if !ok {
			t.Errorf("column=%s missing from initial gauge state", col)
		}
		if v != 0 {
			t.Errorf("column=%s initial value = %v, want 0", col, v)
		}
	}
}

func readGaugeFromFamilies(mfs []*dto.MetricFamily, name string) map[string]float64 {
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		out := map[string]float64{}
		for _, m := range mf.GetMetric() {
			var col string
			for _, lp := range m.GetLabel() {
				if lp.GetName() == "column" {
					col = lp.GetValue()
				}
			}
			out[col] = m.GetGauge().GetValue()
		}
		return out
	}
	return nil
}
