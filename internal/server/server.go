// Package server manages the HTTP listeners for the telemetry service.
// It binds three ports: Tesla (mTLS for vehicle connections), Client
// (WebSocket for browsers), and Metrics (Prometheus + health checks).
// All three are started and stopped together via Start.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/sync/errgroup"

	"github.com/myrobotaxi/telemetry/internal/config"
)

const (
	readHeaderTimeout = 10 * time.Second
	shutdownTimeout   = 15 * time.Second
)

// Server manages the three HTTP listeners (Tesla, Client, Metrics) and
// their lifecycle. Create one with New, call Start to begin serving, and
// cancel the context passed to Start to stop gracefully.
type Server struct {
	tesla          *http.Server
	client         *http.Server
	metrics        *http.Server
	clientMux      *http.ServeMux // stored to allow API route registration
	logger         *slog.Logger
	logMiddleware  func(http.Handler) http.Handler
	teslaPublicKey string // PEM-encoded public key for Tesla .well-known endpoint
}

// New creates a Server with three HTTP servers configured on the ports
// specified in cfg. The metrics server exposes /healthz, /readyz, and
// /metrics. Tesla and client servers use placeholder handlers until wired
// via SetTeslaHandler / SetClientHandler.
// TeslaPublicKey is the PEM-encoded public key served at the .well-known
// endpoint. Pass empty string to disable the endpoint.
func New(cfg config.ServerConfig, logger *slog.Logger, checker ReadinessChecker, reg *prometheus.Registry, teslaPublicKey string) *Server {
	metricsMux := http.NewServeMux()
	metricsMux.HandleFunc("GET /healthz", handleHealthz)
	metricsMux.HandleFunc("GET /readyz", handleReadyz(checker, logger))
	metricsMux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	}))

	skipPaths := map[string]struct{}{
		"/healthz": {},
	}
	logMiddleware := requestLogger(logger, skipPaths)

	// Client mux includes /healthz so the hosting platform (which probes the
	// public port) can run healthchecks without needing access to the metrics port.
	clientMux := http.NewServeMux()
	clientMux.HandleFunc("GET /healthz", handleHealthz)

	if teslaPublicKey != "" {
		registerWellKnown(clientMux, teslaPublicKey)
		logger.Info("Tesla public key endpoint registered at /.well-known/appspecific/com.tesla.3p.public-key.pem")
	} else {
		logger.Warn("TESLA_PUBLIC_KEY not set — .well-known endpoint disabled")
	}

	placeholder := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not implemented", http.StatusNotFound)
	})

	return &Server{
		tesla: &http.Server{
			Addr:              fmt.Sprintf(":%d", cfg.TeslaPort),
			Handler:           logMiddleware(placeholder),
			ReadHeaderTimeout: readHeaderTimeout,
		},
		client: &http.Server{
			Addr:              fmt.Sprintf(":%d", cfg.ClientPort),
			Handler:           logMiddleware(clientMux),
			ReadHeaderTimeout: readHeaderTimeout,
		},
		metrics: &http.Server{
			Addr:              fmt.Sprintf(":%d", cfg.MetricsPort),
			Handler:           logMiddleware(metricsMux),
			ReadHeaderTimeout: readHeaderTimeout,
		},
		clientMux:      clientMux,
		logger:         logger,
		logMiddleware:  logMiddleware,
		teslaPublicKey: teslaPublicKey,
	}
}

// SetTeslaHandler replaces the Tesla server's placeholder handler with
// the given handler (typically the telemetry receiver). Must be called
// before Start.
func (s *Server) SetTeslaHandler(h http.Handler) {
	s.tesla.Handler = s.logMiddleware(h)
}

// SetTeslaTLS configures mTLS on the Tesla server. If set, the Tesla
// port serves TLS and optionally verifies client certificates.
func (s *Server) SetTeslaTLS(tlsConfig *tls.Config) {
	s.tesla.TLSConfig = tlsConfig
}

// SetClientHandler adds the given handler as the catch-all route on the
// client server. The client server always retains /healthz, the Tesla
// .well-known endpoint, and any routes registered via HandleFunc.
// Must be called before Start.
func (s *Server) SetClientHandler(h http.Handler) {
	s.clientMux.Handle("/", h)
	s.client.Handler = s.logMiddleware(s.clientMux)
}

// HandleFunc registers an HTTP handler function on the client server at the
// given pattern (e.g. "POST /api/fleet-config/{vin}"). Must be called
// before Start.
func (s *Server) HandleFunc(pattern string, handler http.HandlerFunc) {
	s.clientMux.HandleFunc(pattern, handler)
}

// Start begins serving on all three ports. It blocks until ctx is
// cancelled or one of the servers returns a fatal error. On context
// cancellation it initiates a graceful shutdown with a fixed timeout.
func (s *Server) Start(ctx context.Context) error {
	g, ctx := errgroup.WithContext(ctx)

	g.Go(func() error { return s.serve(ctx, "tesla", s.tesla) })
	g.Go(func() error { return s.serve(ctx, "client", s.client) })
	g.Go(func() error { return s.serve(ctx, "metrics", s.metrics) })

	if err := g.Wait(); err != nil {
		return fmt.Errorf("server.Start: %w", err)
	}
	return nil
}

// serve starts an HTTP server and shuts it down when ctx is cancelled.
func (s *Server) serve(ctx context.Context, name string, srv *http.Server) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", srv.Addr)
	if err != nil {
		return fmt.Errorf("server.serve(%s): listen %s: %w", name, srv.Addr, err)
	}
	s.logger.Info("server listening",
		slog.String("name", name),
		slog.String("addr", ln.Addr().String()),
	)

	// If TLS is configured, wrap the listener.
	if srv.TLSConfig != nil {
		ln = tls.NewListener(ln, srv.TLSConfig)
		s.logger.Info("TLS enabled", slog.String("name", name))
	}

	errCh := make(chan error, 1)
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("server.serve(%s): %w", name, err)
		}
		close(errCh)
	}()

	select {
	case <-ctx.Done():
		// Context cancelled — initiate shutdown from the Start errgroup.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("server.serve(%s): shutdown: %w", name, err)
		}
		return nil
	case err := <-errCh:
		return err
	}
}

// registerWellKnown adds the Tesla public key endpoint to a mux.
// Tesla Fleet Telemetry verifies app identity by fetching this key.
func registerWellKnown(mux *http.ServeMux, publicKey string) {
	mux.HandleFunc("GET /.well-known/appspecific/com.tesla.3p.public-key.pem", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		fmt.Fprint(w, publicKey)
	})
}
