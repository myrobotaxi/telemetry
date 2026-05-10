package cryptox

import (
	"encoding/base64"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// fakeMetrics is a Metrics test double that records IncDecrypt calls
// per version byte. Used by Encryptor tests that need to assert the
// counter fires on the correct version after a successful Decrypt
// without standing up a Prometheus registry.
type fakeMetrics struct {
	counts map[byte]int
}

func newFakeMetrics() *fakeMetrics {
	return &fakeMetrics{counts: make(map[byte]int)}
}

func (f *fakeMetrics) IncDecrypt(v byte) {
	f.counts[v]++
}

// TestEncryptor_DecryptIncrementsMetricByVersion is the acceptance-
// criterion test from MYR-65: a v1 ciphertext increments the v1 series
// and a v2 ciphertext increments the v2 series, separately. The
// fakeMetrics double makes the assertion direct without depending on
// Prometheus internals.
func TestEncryptor_DecryptIncrementsMetricByVersion(t *testing.T) {
	// KeySet readable under both versions so we can construct a
	// ciphertext under each and decrypt both via one Encryptor.
	bothVersions := newTestKeySet(t, 2, 1, 2)
	v1Only := newTestKeySet(t, 1, 1)

	encV1Writer, err := NewEncryptor(v1Only)
	if err != nil {
		t.Fatalf("NewEncryptor v1 writer: %v", err)
	}
	v1CT, err := encV1Writer.EncryptString("legacy v1 payload")
	if err != nil {
		t.Fatalf("EncryptString v1: %v", err)
	}

	fake := newFakeMetrics()
	enc, err := NewEncryptor(bothVersions, WithMetrics(fake))
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}

	// v2 ciphertext: encrypt under the active write version (v2) of
	// the same Encryptor.
	v2CT, err := enc.EncryptString("modern v2 payload")
	if err != nil {
		t.Fatalf("EncryptString v2: %v", err)
	}

	// Decrypt v1 ciphertext: counter[1]++.
	if _, err := enc.DecryptString(v1CT); err != nil {
		t.Fatalf("DecryptString v1: %v", err)
	}
	if got, want := fake.counts[1], 1; got != want {
		t.Errorf("after v1 decrypt: counts[1] = %d, want %d", got, want)
	}
	if got := fake.counts[2]; got != 0 {
		t.Errorf("after v1 decrypt: counts[2] = %d, want 0", got)
	}

	// Decrypt v2 ciphertext twice: counter[2] = 2; counter[1] still 1.
	if _, err := enc.DecryptString(v2CT); err != nil {
		t.Fatalf("DecryptString v2 (1): %v", err)
	}
	if _, err := enc.DecryptString(v2CT); err != nil {
		t.Fatalf("DecryptString v2 (2): %v", err)
	}
	if got, want := fake.counts[2], 2; got != want {
		t.Errorf("after v2 decrypts: counts[2] = %d, want %d", got, want)
	}
	if got, want := fake.counts[1], 1; got != want {
		t.Errorf("after v2 decrypts: counts[1] = %d, want %d", got, want)
	}
}

// TestEncryptor_FailedDecryptDoesNotIncrement asserts the negative
// requirement from key-rotation.md procedure step 6: a tampered or
// truncated ciphertext, or one whose version byte has no key, MUST
// NOT increment the counter. Otherwise an attacker could mask the
// v1-decay signal during rotation by replaying garbage at the
// decrypt path.
func TestEncryptor_FailedDecryptDoesNotIncrement(t *testing.T) {
	v1Only := newTestKeySet(t, 1, 1)
	fake := newFakeMetrics()
	enc, err := NewEncryptor(v1Only, WithMetrics(fake))
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}

	tests := []struct {
		name       string
		ciphertext string
	}{
		{"empty sentinel skips counter", ""},
		{"invalid base64", "!!!not-base64!!!"},
		{"too short", "AAA="},
		{"unknown version byte", base64.StdEncoding.EncodeToString(unknownVersionBlob())},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _ = enc.DecryptString(tt.ciphertext)
			for v, c := range fake.counts {
				if c != 0 {
					t.Errorf("counts[%d] = %d, want 0 (failed decrypts must not count)", v, c)
				}
			}
		})
	}
}

// TestPrometheusMetrics_RegistersCounterWithPreSeededLabels asserts
// that NewPrometheusMetrics registers cryptox_decrypt_total with one
// version label per readable version, all initialized to zero so a
// /metrics scrape immediately after startup reports the full label
// set. Operators expect zeros, not missing labels, when monitoring
// v1-decay during a rotation.
func TestPrometheusMetrics_RegistersCounterWithPreSeededLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	NewPrometheusMetrics(reg, []byte{1, 2})

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}

	mf := findMetricFamily(mfs, "cryptox_decrypt_total")
	if mf == nil {
		t.Fatal("cryptox_decrypt_total not registered")
	}

	want := map[string]float64{"1": 0, "2": 0}
	if got := len(mf.Metric); got != len(want) {
		t.Fatalf("metric count = %d, want %d", got, len(want))
	}
	for _, m := range mf.Metric {
		v := labelValue(m, "version")
		expected, ok := want[v]
		if !ok {
			t.Errorf("unexpected version label %q", v)
			continue
		}
		if got := m.GetCounter().GetValue(); got != expected {
			t.Errorf("version=%s counter = %v, want %v", v, got, expected)
		}
	}
}

// TestPrometheusMetrics_IncDecryptIncrementsLabeledCounter asserts the
// counter increments on the matching version label and not on other
// labels. End-to-end check from IncDecrypt(byte) through Prometheus
// CounterVec to the gathered output.
func TestPrometheusMetrics_IncDecryptIncrementsLabeledCounter(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewPrometheusMetrics(reg, []byte{1, 2})

	m.IncDecrypt(1)
	m.IncDecrypt(1)
	m.IncDecrypt(2)

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	mf := findMetricFamily(mfs, "cryptox_decrypt_total")
	if mf == nil {
		t.Fatal("cryptox_decrypt_total not registered")
	}

	got := map[string]float64{}
	for _, mt := range mf.Metric {
		got[labelValue(mt, "version")] = mt.GetCounter().GetValue()
	}
	want := map[string]float64{"1": 2, "2": 1}
	for v, expected := range want {
		if got[v] != expected {
			t.Errorf("version=%s counter = %v, want %v", v, got[v], expected)
		}
	}
}

// unknownVersionBlob crafts a minimum-length ciphertext blob whose
// version byte (0xFE) has no key in the test KeySets. Used to drive
// the failure path in DecryptString.
func unknownVersionBlob() []byte {
	b := make([]byte, MinCiphertextLen)
	b[0] = 0xFE
	return b
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
