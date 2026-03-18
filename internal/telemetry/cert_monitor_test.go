package telemetry

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// generateTestCert creates a temporary self-signed certificate file
// with the given validity duration. Returns the file path.
func generateTestCert(t *testing.T, dir string, name string, validity time.Duration) string {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating test key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-" + name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(validity),
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating test certificate: %v", err)
	}

	certPath := filepath.Join(dir, name+".crt")
	f, err := os.Create(certPath)
	if err != nil {
		t.Fatalf("creating cert file: %v", err)
	}
	defer f.Close()

	if err := pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: certDER}); err != nil {
		t.Fatalf("encoding certificate PEM: %v", err)
	}

	return certPath
}

func TestReadCertExpiry(t *testing.T) {
	dir := t.TempDir()

	tests := []struct {
		name      string
		setup     func() string
		wantErr   bool
		checkTime func(t *testing.T, got time.Time)
	}{
		{
			name: "valid certificate",
			setup: func() string {
				return generateTestCert(t, dir, "valid", 30*24*time.Hour)
			},
			checkTime: func(t *testing.T, got time.Time) {
				t.Helper()
				expected := time.Now().Add(30 * 24 * time.Hour)
				diff := got.Sub(expected)
				if diff < -time.Minute || diff > time.Minute {
					t.Errorf("expiry time off by %v, got %v", diff, got)
				}
			},
		},
		{
			name: "expired certificate",
			setup: func() string {
				return generateTestCert(t, dir, "expired", -24*time.Hour)
			},
			checkTime: func(t *testing.T, got time.Time) {
				t.Helper()
				if got.After(time.Now()) {
					t.Errorf("expected expired cert, got future time: %v", got)
				}
			},
		},
		{
			name: "nonexistent file",
			setup: func() string {
				return filepath.Join(dir, "does-not-exist.crt")
			},
			wantErr: true,
		},
		{
			name: "invalid PEM file",
			setup: func() string {
				path := filepath.Join(dir, "invalid.crt")
				if err := os.WriteFile(path, []byte("not a PEM file"), 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
			wantErr: true,
		},
		{
			name: "invalid certificate in PEM",
			setup: func() string {
				path := filepath.Join(dir, "bad-cert.crt")
				block := &pem.Block{Type: "CERTIFICATE", Bytes: []byte("garbage")}
				data := pem.EncodeToMemory(block)
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
				return path
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.setup()
			got, err := readCertExpiry(path)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.checkTime != nil {
				tt.checkTime(t, got)
			}
		})
	}
}

func TestCertMonitorCheckAll(t *testing.T) {
	dir := t.TempDir()

	validPath := generateTestCert(t, dir, "server", 90*24*time.Hour)
	expiringPath := generateTestCert(t, dir, "expiring", 7*24*time.Hour)

	reg := prometheus.NewRegistry()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	monitor := NewCertMonitor(CertMonitorConfig{
		CertPaths: map[string]string{
			"server":   validPath,
			"expiring": expiringPath,
		},
		CheckInterval: time.Hour,
	}, reg, logger)

	// Run a single check.
	monitor.checkAll()

	// Gather all metrics and verify.
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}

	metrics := make(map[string]map[string]float64) // metric_name → cert_label → value
	for _, family := range families {
		name := family.GetName()
		metrics[name] = make(map[string]float64)
		for _, m := range family.GetMetric() {
			for _, label := range m.GetLabel() {
				if label.GetName() == "cert" {
					metrics[name][label.GetValue()] = m.GetGauge().GetValue()
				}
			}
		}
	}

	// Check expiry timestamp metrics.
	expiryMetrics := metrics["telemetry_tls_cert_expiry_timestamp_seconds"]
	if expiryMetrics["server"] == 0 {
		t.Error("server cert expiry timestamp should be non-zero")
	}
	if expiryMetrics["expiring"] == 0 {
		t.Error("expiring cert expiry timestamp should be non-zero")
	}

	// Check days remaining metrics.
	daysMetrics := metrics["telemetry_tls_cert_expiry_days_remaining"]
	serverDays := daysMetrics["server"]
	if serverDays < 85 || serverDays > 95 {
		t.Errorf("server cert days remaining should be ~90, got %v", serverDays)
	}

	expiringDays := daysMetrics["expiring"]
	if expiringDays < 3 || expiringDays > 10 {
		t.Errorf("expiring cert days remaining should be ~7, got %v", expiringDays)
	}
}

func TestCertMonitorMissingFile(t *testing.T) {
	reg := prometheus.NewRegistry()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	monitor := NewCertMonitor(CertMonitorConfig{
		CertPaths: map[string]string{
			"missing": "/nonexistent/cert.crt",
		},
		CheckInterval: time.Hour,
	}, reg, logger)

	// Should not panic on missing file.
	monitor.checkAll()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}

	for _, family := range families {
		for _, m := range family.GetMetric() {
			if m.GetGauge().GetValue() != 0 {
				t.Errorf("expected gauge value 0 for missing cert, got %v", m.GetGauge().GetValue())
			}
		}
	}
}

func TestCertMonitorRunCancellation(t *testing.T) {
	dir := t.TempDir()
	certPath := generateTestCert(t, dir, "cancel-test", 30*24*time.Hour)

	reg := prometheus.NewRegistry()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	monitor := NewCertMonitor(CertMonitorConfig{
		CertPaths:     map[string]string{"test": certPath},
		CheckInterval: 100 * time.Millisecond,
	}, reg, logger)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		monitor.Run(ctx)
		close(done)
	}()

	// Let it run for a couple of ticks.
	time.Sleep(250 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// Good, Run returned after cancellation.
	case <-time.After(2 * time.Second):
		t.Fatal("CertMonitor.Run did not return after context cancellation")
	}

	// Verify metrics were populated.
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}

	found := false
	for _, family := range families {
		if family.GetName() == "telemetry_tls_cert_expiry_timestamp_seconds" {
			for _, m := range family.GetMetric() {
				if gaugeValue(m) > 0 {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("expected cert expiry metric to be set after Run")
	}
}

func gaugeValue(m *dto.Metric) float64 {
	if m.GetGauge() != nil {
		return m.GetGauge().GetValue()
	}
	return 0
}
