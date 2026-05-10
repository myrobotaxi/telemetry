package cryptox

import (
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the observability hook the Encryptor calls on every
// successful Decrypt. The version label is the ciphertext version byte
// that produced the plaintext — operators rely on this counter during
// key rotation to confirm ciphertexts under a retiring version have
// drained to zero before the key is removed (see
// docs/contracts/key-rotation.md §"Procedure" step 6).
//
// Library users that don't run a Prometheus registry pass NoopMetrics
// (the default — NewEncryptor uses NoopMetrics if no option is given).
// The composition root in cmd/telemetry-server wires PrometheusMetrics
// against the existing /metrics registry.
//
// IncDecrypt is intentionally fire-and-forget: it is invoked on the
// hot path of every persisted-row read, must never block, and must
// never return an error. Implementations that need to surface failures
// should log internally.
type Metrics interface {
	// IncDecrypt records one successful decrypt of a ciphertext stamped
	// with the given version byte. Called only after AES-GCM Open
	// succeeds — failed decrypts (auth failures, unknown version,
	// truncated input) MUST NOT count.
	IncDecrypt(version byte)
}

// NoopMetrics is the zero-cost default. Use it in unit tests, library
// consumers without a Prometheus registry, or anywhere the metric is
// not desired. The empty struct allocates nothing.
type NoopMetrics struct{}

// IncDecrypt is a no-op.
func (NoopMetrics) IncDecrypt(byte) {}

// PrometheusMetrics is the Encryptor.Metrics implementation backed by a
// Prometheus CounterVec labeled by ciphertext version. The counter
// `cryptox_decrypt_total{version="N"}` increments per successful
// decrypt; operators read the v1 series during a v1→v2 rotation to
// confirm decay to zero before retiring v1.
type PrometheusMetrics struct {
	counter *prometheus.CounterVec
}

// NewPrometheusMetrics registers the counter on reg and pre-creates the
// label series for the supplied versions so every label is visible on
// the first /metrics scrape (operators expect zero values, not missing
// labels — a missing label is indistinguishable from a never-seen
// version, which is what we're rotating away from). The KeySet's
// readable versions are the authoritative source — pass
// KeySet.ReadableVersions() at startup.
func NewPrometheusMetrics(reg prometheus.Registerer, versions []byte) *PrometheusMetrics {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "cryptox_decrypt_total",
		Help: "Successful AES-256-GCM decrypts by ciphertext version byte. During key rotation, the retiring version's series should decay to zero before the key is removed (see docs/contracts/key-rotation.md).",
	}, []string{"version"})
	reg.MustRegister(c)

	// Pre-register every known version so /metrics shows the full set
	// of labels on the first scrape. Without this, a label only
	// appears after its first decrypt, which races with operator
	// dashboards during rotation.
	for _, v := range versions {
		c.WithLabelValues(versionLabel(v)).Add(0)
	}

	return &PrometheusMetrics{counter: c}
}

// IncDecrypt increments the version-labeled counter by 1.
func (m *PrometheusMetrics) IncDecrypt(version byte) {
	m.counter.WithLabelValues(versionLabel(version)).Inc()
}

// versionLabel renders a version byte as its decimal string form so
// the metric label matches the operator-facing convention in
// key-rotation.md (`version="1"`, `version="2"`, ...).
func versionLabel(v byte) string {
	return strconv.Itoa(int(v))
}
