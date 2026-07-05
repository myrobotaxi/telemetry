package telemetry

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Default cadence and per-probe timeout for the endpoint cert monitor.
const (
	defaultEndpointCheckInterval = 6 * time.Hour
	defaultEndpointDialTimeout   = 10 * time.Second

	// Day thresholds at which a served cert's remaining validity escalates
	// healthy → warn → critical in the logs. The Prometheus alert rules in
	// deployments/alerts/tls-cert.rules.yml mirror these numbers.
	certWarnDays     = 30
	certCriticalDays = 14
)

// EndpointCertMonitor periodically TLS-dials a set of live endpoints and
// exposes the *served* leaf certificate's expiry as Prometheus gauges.
//
// Unlike the file-based CertMonitor, this reads the certificate the server
// actually presents on the wire, so it covers TLS terminated OUTSIDE the
// app process — e.g. the Fly-managed cert on the client WebSocket port
// (4443), which no on-disk file monitor can ever see. That blind spot is
// exactly what let the 4443 cert lapse unnoticed in the 2026-07 outage
// (MYR-188).
type EndpointCertMonitor struct {
	expiryGauge    *prometheus.GaugeVec
	daysGauge      *prometheus.GaugeVec
	reachableGauge *prometheus.GaugeVec
	endpoints      []string
	dialTimeout    time.Duration
	checkInterval  time.Duration
	logger         *slog.Logger
}

// EndpointCertMonitorConfig configures an EndpointCertMonitor.
type EndpointCertMonitorConfig struct {
	// Endpoints is the list of host:port addresses to TLS-dial. Example:
	// {"telemetry.myrobotaxi.app:443", "telemetry.myrobotaxi.app:4443"}.
	Endpoints []string

	// CheckInterval is how often to re-probe every endpoint. Default: 6h.
	CheckInterval time.Duration

	// DialTimeout bounds each individual TLS handshake. Default: 10s.
	DialTimeout time.Duration
}

// NewEndpointCertMonitor creates an EndpointCertMonitor and registers its
// Prometheus metrics on reg.
func NewEndpointCertMonitor(cfg EndpointCertMonitorConfig, reg prometheus.Registerer, logger *slog.Logger) *EndpointCertMonitor {
	if cfg.CheckInterval == 0 {
		cfg.CheckInterval = defaultEndpointCheckInterval
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = defaultEndpointDialTimeout
	}

	m := &EndpointCertMonitor{
		expiryGauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "telemetry",
			Subsystem: "tls",
			Name:      "endpoint_cert_expiry_timestamp_seconds",
			Help:      "Unix timestamp when the certificate served at the endpoint expires.",
		}, []string{"endpoint"}),
		daysGauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "telemetry",
			Subsystem: "tls",
			Name:      "endpoint_cert_expiry_days_remaining",
			Help:      "Days until the certificate served at the endpoint expires.",
		}, []string{"endpoint"}),
		reachableGauge: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "telemetry",
			Subsystem: "tls",
			Name:      "endpoint_cert_reachable",
			Help:      "1 if the endpoint's certificate was read on the last probe, else 0.",
		}, []string{"endpoint"}),
		endpoints:     cfg.Endpoints,
		dialTimeout:   cfg.DialTimeout,
		checkInterval: cfg.CheckInterval,
		logger:        logger,
	}

	reg.MustRegister(m.expiryGauge, m.daysGauge, m.reachableGauge)
	return m
}

// Run starts the periodic probe loop. It blocks until ctx is cancelled;
// call it in a goroutine.
func (m *EndpointCertMonitor) Run(ctx context.Context) {
	m.checkAll(ctx)

	ticker := time.NewTicker(m.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkAll(ctx)
		}
	}
}

// checkAll probes every configured endpoint once and updates metrics.
func (m *EndpointCertMonitor) checkAll(ctx context.Context) {
	for _, ep := range m.endpoints {
		m.checkOne(ctx, ep)
	}
}

// checkOne TLS-dials a single endpoint, records the served leaf's expiry,
// and logs an escalating message as the remaining validity shrinks.
func (m *EndpointCertMonitor) checkOne(ctx context.Context, endpoint string) {
	expiry, err := m.probeExpiry(ctx, endpoint)
	if err != nil {
		m.logger.Error("tls endpoint cert probe failed",
			slog.String("endpoint", endpoint),
			slog.String("error", err.Error()),
		)
		m.reachableGauge.WithLabelValues(endpoint).Set(0)
		// Deliberately leave the expiry/days gauges at their last known
		// value rather than zeroing them: a transient dial failure must
		// not read as "expires at epoch 0" and page on its own. The
		// reachable gauge going to 0 is the signal for an unreachable
		// endpoint; a separate alert rule watches it.
		return
	}

	m.reachableGauge.WithLabelValues(endpoint).Set(1)
	m.expiryGauge.WithLabelValues(endpoint).Set(float64(expiry.Unix()))

	days := time.Until(expiry).Hours() / 24
	m.daysGauge.WithLabelValues(endpoint).Set(days)

	switch {
	case days < certCriticalDays:
		m.logger.Error("tls endpoint cert expiring imminently",
			slog.String("endpoint", endpoint),
			slog.Float64("days_remaining", days),
			slog.Time("expires", expiry),
		)
	case days < certWarnDays:
		m.logger.Warn("tls endpoint cert expiring soon",
			slog.String("endpoint", endpoint),
			slog.Float64("days_remaining", days),
			slog.Time("expires", expiry),
		)
	default:
		m.logger.Debug("tls endpoint cert checked",
			slog.String("endpoint", endpoint),
			slog.Float64("days_remaining", days),
			slog.Time("expires", expiry),
		)
	}
}

// probeExpiry TLS-dials endpoint and returns the served leaf certificate's
// NotAfter.
//
// Certificate verification is intentionally disabled: the entire point is
// to read the expiry of whatever cert is served, including an expired or
// hostname-mismatched one — a validating dial would fail the handshake and
// tell us nothing. The served leaf is captured via a VerifyConnection hook
// (which runs regardless of InsecureSkipVerify, and on resumed sessions) so
// that even the Tesla mTLS port (443), which aborts the handshake because
// we present no client certificate, still yields the server cert it sent
// before aborting.
func (m *EndpointCertMonitor) probeExpiry(ctx context.Context, endpoint string) (time.Time, error) {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing endpoint %q: %w", endpoint, err)
	}

	var leafExpiry time.Time
	capture := func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) > 0 {
			leafExpiry = cs.PeerCertificates[0].NotAfter
		}
		return nil
	}

	dialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: m.dialTimeout},
		Config: &tls.Config{
			ServerName: host, // SNI — Fly serves the right cert per hostname
			// #nosec G402 -- this is a monitoring probe: we read the served
			// cert's expiry, so an expired/invalid/mismatched cert MUST be
			// inspected, not rejected. A validating dial would fail the
			// handshake and defeat the entire purpose. (//nolint covers the
			// golangci-lint gosec pass; #nosec covers the standalone gosec CI job.)
			InsecureSkipVerify: true, //nolint:gosec // G402: intentional — see #nosec note above
			MinVersion:         tls.VersionTLS12,
			VerifyConnection:   capture,
		},
	}

	dialCtx, cancel := context.WithTimeout(ctx, m.dialTimeout)
	defer cancel()

	conn, dialErr := dialer.DialContext(dialCtx, "tcp", endpoint)
	if conn != nil {
		_ = conn.Close()
	}

	// The mTLS listener (443) requires a client cert we don't present, so
	// DialContext returns an error AFTER the server has already sent its
	// certificate — which the capture hook recorded. Prefer the captured
	// expiry; only surface the dial error when we captured nothing.
	switch {
	case !leafExpiry.IsZero():
		return leafExpiry, nil
	case dialErr != nil:
		return time.Time{}, fmt.Errorf("tls dial %s: %w", endpoint, dialErr)
	default:
		return time.Time{}, fmt.Errorf("endpoint %s presented no certificate", endpoint)
	}
}
