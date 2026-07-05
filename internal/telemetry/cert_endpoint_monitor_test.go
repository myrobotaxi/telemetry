package telemetry

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log/slog"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// startTestTLSServer starts a TLS listener on 127.0.0.1 serving a
// self-signed cert with the given validity and client-auth policy, and
// returns its host:port. The listener is closed on test cleanup.
func startTestTLSServer(t *testing.T, validity time.Duration, clientAuth tls.ClientAuthType) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(validity),
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating cert: %v", err)
	}

	cfg := &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		MinVersion:   tls.VersionTLS12,
		ClientAuth:   clientAuth,
	}
	if clientAuth == tls.RequireAndVerifyClientCert {
		// A CA pool the (absent) client cert can never satisfy, forcing the
		// server to abort the handshake AFTER it has already sent its own
		// certificate — mirrors the Tesla mTLS (443) path the monitor must
		// still be able to read.
		pool := x509.NewCertPool()
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatalf("parsing test cert: %v", err)
		}
		pool.AddCert(leaf)
		cfg.ClientCAs = pool
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				if tc, ok := c.(*tls.Conn); ok {
					_ = tc.HandshakeContext(context.Background())
				}
				_ = c.Close()
			}(conn)
		}
	}()

	return ln.Addr().String()
}

func newTestEndpointMonitor(t *testing.T, endpoints []string, reg *prometheus.Registry) *EndpointCertMonitor {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	return NewEndpointCertMonitor(EndpointCertMonitorConfig{
		Endpoints:   endpoints,
		DialTimeout: 3 * time.Second,
	}, reg, logger)
}

func TestEndpointCertMonitorProbeExpiry(t *testing.T) {
	valid := startTestTLSServer(t, 90*24*time.Hour, tls.NoClientCert)
	mtls := startTestTLSServer(t, 45*24*time.Hour, tls.RequireAndVerifyClientCert)

	tests := []struct {
		name     string
		endpoint string
		wantErr  bool
		wantDays float64
	}{
		{name: "valid server", endpoint: valid, wantDays: 90},
		// The server requires a client cert we never present, so the
		// handshake aborts — but the served leaf must still be read.
		{name: "mtls server still readable", endpoint: mtls, wantDays: 45},
		{name: "connection refused", endpoint: "127.0.0.1:1", wantErr: true},
		{name: "missing port", endpoint: "no-port", wantErr: true},
	}

	m := newTestEndpointMonitor(t, nil, prometheus.NewRegistry())
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := m.probeExpiry(context.Background(), tt.endpoint)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got expiry %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			days := time.Until(got).Hours() / 24
			if days < tt.wantDays-2 || days > tt.wantDays+2 {
				t.Errorf("days remaining = %v, want ~%v", days, tt.wantDays)
			}
		})
	}
}

func TestEndpointCertMonitorCheckAll(t *testing.T) {
	valid := startTestTLSServer(t, 90*24*time.Hour, tls.NoClientCert)
	const unreachable = "127.0.0.1:1"

	reg := prometheus.NewRegistry()
	m := newTestEndpointMonitor(t, []string{valid, unreachable}, reg)

	m.checkAll(context.Background())

	gauges := gatherEndpointGauges(t, reg)

	if got := gauges["telemetry_tls_endpoint_cert_reachable"][valid]; got != 1 {
		t.Errorf("valid endpoint reachable = %v, want 1", got)
	}
	if got := gauges["telemetry_tls_endpoint_cert_reachable"][unreachable]; got != 0 {
		t.Errorf("unreachable endpoint reachable = %v, want 0", got)
	}
	days := gauges["telemetry_tls_endpoint_cert_expiry_days_remaining"][valid]
	if days < 85 || days > 95 {
		t.Errorf("valid endpoint days remaining = %v, want ~90", days)
	}
	if ts := gauges["telemetry_tls_endpoint_cert_expiry_timestamp_seconds"][valid]; ts == 0 {
		t.Error("valid endpoint expiry timestamp should be non-zero")
	}
}

func TestEndpointCertMonitorRunCancellation(t *testing.T) {
	valid := startTestTLSServer(t, 30*24*time.Hour, tls.NoClientCert)

	reg := prometheus.NewRegistry()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	m := NewEndpointCertMonitor(EndpointCertMonitorConfig{
		Endpoints:     []string{valid},
		CheckInterval: 50 * time.Millisecond,
		DialTimeout:   3 * time.Second,
	}, reg, logger)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after context cancellation")
	}
}

// gatherEndpointGauges returns metric_name → endpoint_label → value for
// every gauge in reg that carries an "endpoint" label.
func gatherEndpointGauges(t *testing.T, reg *prometheus.Registry) map[string]map[string]float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	out := make(map[string]map[string]float64)
	for _, f := range families {
		vals := make(map[string]float64)
		for _, metric := range f.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetName() == "endpoint" {
					vals[label.GetValue()] = metric.GetGauge().GetValue()
				}
			}
		}
		out[f.GetName()] = vals
	}
	return out
}
