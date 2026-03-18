package telemetry

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// CertMonitor periodically reads TLS certificate files and exposes their
// expiry timestamps as Prometheus gauges. This enables alerting on
// certificates that are about to expire (critical for Tesla mTLS which
// silently fails on expired certs).
type CertMonitor struct {
	expiryGauge   *prometheus.GaugeVec
	daysGauge     *prometheus.GaugeVec
	certPaths     map[string]string // label → file path
	checkInterval time.Duration
	logger        *slog.Logger
	mu            sync.RWMutex
}

// CertMonitorConfig holds configuration for the certificate monitor.
type CertMonitorConfig struct {
	// CertPaths maps a human-readable label to the certificate file path.
	// Example: {"server": "/certs/server.crt", "client": "/certs/client.crt"}
	CertPaths map[string]string

	// CheckInterval is how often to re-read cert files. Default: 1 hour.
	CheckInterval time.Duration
}

// NewCertMonitor creates a CertMonitor and registers its Prometheus metrics.
func NewCertMonitor(cfg CertMonitorConfig, reg prometheus.Registerer, logger *slog.Logger) *CertMonitor {
	if cfg.CheckInterval == 0 {
		cfg.CheckInterval = time.Hour
	}

	m := &CertMonitor{
		expiryGauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "telemetry",
			Subsystem: "tls",
			Name:      "cert_expiry_timestamp_seconds",
			Help:      "Unix timestamp when the TLS certificate expires.",
		}, []string{"cert"}),
		daysGauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "telemetry",
			Subsystem: "tls",
			Name:      "cert_expiry_days_remaining",
			Help:      "Number of days until the TLS certificate expires.",
		}, []string{"cert"}),
		certPaths:     cfg.CertPaths,
		checkInterval: cfg.CheckInterval,
		logger:        logger,
	}

	reg.MustRegister(m.expiryGauge, m.daysGauge)
	return m
}

// Run starts the periodic certificate check loop. It blocks until the
// context is cancelled. Call this in a goroutine.
func (m *CertMonitor) Run(ctx context.Context) {
	// Do an initial check immediately.
	m.checkAll()

	ticker := time.NewTicker(m.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkAll()
		}
	}
}

// checkAll reads all configured certificate files and updates metrics.
func (m *CertMonitor) checkAll() {
	m.mu.RLock()
	paths := m.certPaths
	m.mu.RUnlock()

	for label, path := range paths {
		expiry, err := readCertExpiry(path)
		if err != nil {
			m.logger.Warn("failed to read certificate expiry",
				slog.String("cert", label),
				slog.String("path", path),
				slog.String("error", err.Error()),
			)
			// Set gauge to 0 to indicate a problem.
			m.expiryGauge.WithLabelValues(label).Set(0)
			m.daysGauge.WithLabelValues(label).Set(0)
			continue
		}

		m.expiryGauge.WithLabelValues(label).Set(float64(expiry.Unix()))

		daysRemaining := time.Until(expiry).Hours() / 24
		m.daysGauge.WithLabelValues(label).Set(daysRemaining)

		m.logger.Debug("certificate expiry checked",
			slog.String("cert", label),
			slog.Float64("days_remaining", daysRemaining),
			slog.Time("expires", expiry),
		)
	}
}

// readCertExpiry reads a PEM-encoded certificate file and returns its
// NotAfter timestamp.
func readCertExpiry(path string) (time.Time, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path is operator-configured cert file, not user input
	if err != nil {
		return time.Time{}, fmt.Errorf("reading cert file %s: %w", path, err)
	}

	block, _ := pem.Decode(data)
	if block == nil {
		return time.Time{}, fmt.Errorf("no PEM block found in %s", path)
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing certificate from %s: %w", path, err)
	}

	return cert.NotAfter, nil
}
