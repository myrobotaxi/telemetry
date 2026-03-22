package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/tnando/my-robo-taxi-telemetry/internal/config"
)

// freePorts returns n available TCP ports by binding to :0 and releasing.
func freePorts(t *testing.T, n int) []int {
	t.Helper()
	ports := make([]int, n)
	for i := range n {
		var lc net.ListenConfig
		ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("freePorts: %v", err)
		}
		ports[i] = ln.Addr().(*net.TCPAddr).Port
		ln.Close()
	}
	return ports
}

// testServerConfig returns a ServerConfig using available ports.
func testServerConfig(t *testing.T) config.ServerConfig {
	t.Helper()
	ports := freePorts(t, 3)
	return config.ServerConfig{
		TeslaPort:   ports[0],
		ClientPort:  ports[1],
		MetricsPort: ports[2],
	}
}

func TestServer_StartShutdown(t *testing.T) {
	cfg := testServerConfig(t)
	reg := prometheus.NewRegistry()
	checker := &mockChecker{}

	srv := New(cfg, testLogger(), checker, reg, "")

	ctx, cancel := context.WithCancel(context.Background())

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start(ctx)
	}()

	// Wait for servers to be ready by polling the healthz endpoint.
	metricsAddr := fmt.Sprintf("http://127.0.0.1:%d", cfg.MetricsPort)
	if err := waitForReady(t, metricsAddr+"/healthz"); err != nil {
		t.Fatalf("metrics server did not become ready: %v", err)
	}

	// Cancel context to trigger shutdown.
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Start returned error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after context cancellation")
	}
}

func TestServer_HealthzEndpoint(t *testing.T) {
	cfg := testServerConfig(t)
	reg := prometheus.NewRegistry()
	checker := &mockChecker{}

	srv := New(cfg, testLogger(), checker, reg, "")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start(ctx)
	}()

	metricsAddr := fmt.Sprintf("http://127.0.0.1:%d", cfg.MetricsPort)
	if err := waitForReady(t, metricsAddr+"/healthz"); err != nil {
		t.Fatalf("server did not become ready: %v", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, metricsAddr+"/healthz", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status code: got %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var body healthResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status: got %q, want %q", body.Status, "ok")
	}
}

func TestServer_WellKnownEndpoint(t *testing.T) {
	cfg := testServerConfig(t)
	reg := prometheus.NewRegistry()
	checker := &mockChecker{}
	testKey := "-----BEGIN PUBLIC KEY-----\nTESTKEY\n-----END PUBLIC KEY-----"

	srv := New(cfg, testLogger(), checker, reg, testKey)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() { _ = srv.Start(ctx) }()

	clientAddr := fmt.Sprintf("http://127.0.0.1:%d", cfg.ClientPort)
	if err := waitForReady(t, clientAddr+"/healthz"); err != nil {
		t.Fatalf("server did not become ready: %v", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, clientAddr+"/.well-known/appspecific/com.tesla.3p.public-key.pem", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET .well-known: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/x-pem-file" {
		t.Errorf("content-type = %q, want application/x-pem-file", ct)
	}

	body, _ := io.ReadAll(resp.Body)
	if string(body) != testKey {
		t.Errorf("body = %q, want %q", string(body), testKey)
	}
}

func TestServer_WellKnownSurvivesSetClientHandler(t *testing.T) {
	cfg := testServerConfig(t)
	reg := prometheus.NewRegistry()
	checker := &mockChecker{}
	testKey := "-----BEGIN PUBLIC KEY-----\nTESTKEY\n-----END PUBLIC KEY-----"

	srv := New(cfg, testLogger(), checker, reg, testKey)
	// SetClientHandler should NOT drop the .well-known route
	srv.SetClientHandler(http.NotFoundHandler())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() { _ = srv.Start(ctx) }()

	clientAddr := fmt.Sprintf("http://127.0.0.1:%d", cfg.ClientPort)
	if err := waitForReady(t, clientAddr+"/healthz"); err != nil {
		t.Fatalf("server did not become ready: %v", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, clientAddr+"/.well-known/appspecific/com.tesla.3p.public-key.pem", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET .well-known after SetClientHandler: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (route should survive SetClientHandler)", resp.StatusCode)
	}
}

func TestServer_WellKnownDisabledWhenKeyEmpty(t *testing.T) {
	cfg := testServerConfig(t)
	reg := prometheus.NewRegistry()
	checker := &mockChecker{}

	srv := New(cfg, testLogger(), checker, reg, "") // empty key

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go func() { _ = srv.Start(ctx) }()

	clientAddr := fmt.Sprintf("http://127.0.0.1:%d", cfg.ClientPort)
	if err := waitForReady(t, clientAddr+"/healthz"); err != nil {
		t.Fatalf("server did not become ready: %v", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, clientAddr+"/.well-known/appspecific/com.tesla.3p.public-key.pem", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET .well-known: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Error("expected non-200 when key is empty, got 200")
	}
}

func TestServer_HealthzOnClientPort(t *testing.T) {
	cfg := testServerConfig(t)
	reg := prometheus.NewRegistry()
	checker := &mockChecker{}

	srv := New(cfg, testLogger(), checker, reg, "")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start(ctx)
	}()

	clientAddr := fmt.Sprintf("http://127.0.0.1:%d", cfg.ClientPort)
	if err := waitForReady(t, clientAddr+"/healthz"); err != nil {
		t.Fatalf("client port healthz did not become ready: %v", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, clientAddr+"/healthz", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /healthz on client port: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status code: got %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestServer_ReadyzEndpoint(t *testing.T) {
	tests := []struct {
		name       string
		pingErr    error
		wantStatus int
		wantBody   string
	}{
		{
			name:       "ready when checker succeeds",
			pingErr:    nil,
			wantStatus: http.StatusOK,
			wantBody:   "ready",
		},
		{
			name:       "not ready when checker fails",
			pingErr:    fmt.Errorf("store.DB.Ping: connection refused"),
			wantStatus: http.StatusServiceUnavailable,
			wantBody:   "not ready",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := testServerConfig(t)
			reg := prometheus.NewRegistry()
			checker := &mockChecker{err: tt.pingErr}

			srv := New(cfg, testLogger(), checker, reg, "")

			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			errCh := make(chan error, 1)
			go func() {
				errCh <- srv.Start(ctx)
			}()

			metricsAddr := fmt.Sprintf("http://127.0.0.1:%d", cfg.MetricsPort)
			if err := waitForReady(t, metricsAddr+"/healthz"); err != nil {
				t.Fatalf("server did not become ready: %v", err)
			}

			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, metricsAddr+"/readyz", nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET /readyz: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tt.wantStatus {
				t.Errorf("status code: got %d, want %d", resp.StatusCode, tt.wantStatus)
			}

			var body healthResponse
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body.Status != tt.wantBody {
				t.Errorf("status: got %q, want %q", body.Status, tt.wantBody)
			}
		})
	}
}

func TestServer_MetricsEndpoint(t *testing.T) {
	cfg := testServerConfig(t)
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	checker := &mockChecker{}

	srv := New(cfg, testLogger(), checker, reg, "")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start(ctx)
	}()

	metricsAddr := fmt.Sprintf("http://127.0.0.1:%d", cfg.MetricsPort)
	if err := waitForReady(t, metricsAddr+"/healthz"); err != nil {
		t.Fatalf("server did not become ready: %v", err)
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, metricsAddr+"/metrics", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status code: got %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	// Prometheus response should contain standard Go runtime metrics.
	bodyStr := string(body)
	if len(bodyStr) == 0 {
		t.Error("empty metrics response")
	}
}

func TestServer_PlaceholderEndpoints(t *testing.T) {
	cfg := testServerConfig(t)
	reg := prometheus.NewRegistry()
	checker := &mockChecker{}

	srv := New(cfg, testLogger(), checker, reg, "")

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Start(ctx)
	}()

	metricsAddr := fmt.Sprintf("http://127.0.0.1:%d", cfg.MetricsPort)
	if err := waitForReady(t, metricsAddr+"/healthz"); err != nil {
		t.Fatalf("server did not become ready: %v", err)
	}

	// Tesla port should return 404 (placeholder).
	teslaAddr := fmt.Sprintf("http://127.0.0.1:%d", cfg.TeslaPort)
	teslaReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, teslaAddr+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := http.DefaultClient.Do(teslaReq)
	if err != nil {
		t.Fatalf("GET tesla /: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("tesla status: got %d, want %d", resp.StatusCode, http.StatusNotFound)
	}

	// Client port should return 404 (placeholder).
	clientAddr := fmt.Sprintf("http://127.0.0.1:%d", cfg.ClientPort)
	clientReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, clientAddr+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err = http.DefaultClient.Do(clientReq)
	if err != nil {
		t.Fatalf("GET client /: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("client status: got %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

const waitForReadyTimeout = 10 * time.Second

// waitForReady polls the given URL until it returns 200 or the timeout elapses.
func waitForReady(t *testing.T, url string) error {
	t.Helper()
	deadline := time.After(waitForReadyTimeout)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			return fmt.Errorf("timeout waiting for %s", url)
		case <-ticker.C:
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
			if err != nil {
				return fmt.Errorf("waitForReady: new request: %w", err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				continue
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
	}
}
