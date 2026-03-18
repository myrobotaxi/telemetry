// Binary telemetry-server receives real-time vehicle telemetry from Tesla's
// Fleet Telemetry system and broadcasts it to connected browser clients via
// WebSocket. This file is the composition root — it wires dependencies and
// starts the server. No business logic lives here.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/tnando/my-robo-taxi-telemetry/internal/config"
	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
	"github.com/tnando/my-robo-taxi-telemetry/internal/server"
	"github.com/tnando/my-robo-taxi-telemetry/internal/store"
	"github.com/tnando/my-robo-taxi-telemetry/internal/telemetry"
)

// Build-time variables set via ldflags (see .goreleaser.yml).
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "telemetry-server: %s\n", err)
		os.Exit(1)
	}
}

func run() error {
	// --- Flag parsing ---
	var (
		configPath = flag.String("config", "", "path to JSON configuration file")
		logLevel   = flag.String("log-level", "info", "log level: debug, info, warn, error")
	)
	flag.Parse()

	// --- Logger setup ---
	logger, err := newLogger(*logLevel)
	if err != nil {
		return fmt.Errorf("initializing logger: %w", err)
	}
	slog.SetDefault(logger)

	logger.Info("starting telemetry-server",
		slog.String("version", version),
		slog.String("commit", commit),
		slog.String("date", date),
		slog.String("config", *configPath),
	)

	// --- Context with signal-based cancellation ---
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// --- Configuration loading ---
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	logger.Info("configuration loaded",
		slog.Int("tesla_port", cfg.Server().TeslaPort),
		slog.Int("client_port", cfg.Server().ClientPort),
		slog.Int("metrics_port", cfg.Server().MetricsPort),
	)

	// --- Prometheus registry ---
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	// --- Database connection ---
	db, err := store.NewDB(ctx, cfg.Database(), logger.With(slog.String("component", "store")), store.NoopMetrics{})
	if err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer db.Close()

	// --- Event bus ---
	bus := events.NewChannelBus(events.BusConfig{
		BufferSize: cfg.Telemetry().EventBufferSize,
	}, events.NoopBusMetrics{}, logger.With(slog.String("component", "events")))

	// --- Telemetry receiver ---
	recv := telemetry.NewReceiver(
		telemetry.NewDecoder(),
		bus,
		logger.With(slog.String("component", "receiver")),
		telemetry.NoopReceiverMetrics{},
		telemetry.ReceiverConfig{
			MaxVehicles:       cfg.Telemetry().MaxVehicles,
			MaxMessagesPerSec: 10,
		},
	)

	// TODO: Initialize drive detector (subscribes to telemetry events).
	// TODO: Initialize client WebSocket server (subscribes to events, pushes to browsers).
	// TODO: Initialize auth middleware.

	// --- HTTP servers ---
	srv := server.New(cfg.Server(), logger, db, reg)
	srv.SetTeslaHandler(recv.Handler())

	// Configure mTLS on Tesla port if TLS cert files are available.
	if cfg.TLS().CertFile != "" && cfg.TLS().KeyFile != "" {
		teslaTLS, err := buildTeslaTLS(cfg.TLS())
		if err != nil {
			logger.Warn("TLS not configured for Tesla port, skipping mTLS", slog.Any("error", err))
		} else {
			srv.SetTeslaTLS(teslaTLS)
		}
	}

	logger.Info("starting HTTP servers")
	if err := srv.Start(ctx); err != nil {
		return fmt.Errorf("server: %w", err)
	}

	logger.Info("telemetry-server stopped cleanly")
	return nil
}

func newLogger(level string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("parsing log level %q: %w", level, err)
	}

	// Use text handler for local development (human-readable).
	// In production, set LOG_FORMAT=json or swap to slog.NewJSONHandler.
	var handler slog.Handler
	if os.Getenv("LOG_FORMAT") == "json" {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	} else {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	}

	return slog.New(handler), nil
}

// buildTeslaTLS creates a TLS config for the Tesla mTLS port.
// It loads the server cert/key and optionally a CA for verifying client certs.
// If no CA file is configured, client certs are requested but not verified
// (suitable for local dev with self-signed certs).
func buildTeslaTLS(cfg config.TLSConfig) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("loading server cert: %w", err)
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequestClientCert,
		MinVersion:   tls.VersionTLS12,
	}

	if cfg.CAFile != "" {
		caPEM, err := os.ReadFile(cfg.CAFile) // #nosec G304 -- operator-configured cert path
		if err != nil {
			return nil, fmt.Errorf("reading CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caPEM) {
			return nil, fmt.Errorf("no valid certs found in CA file %s", cfg.CAFile)
		}
		tlsCfg.ClientCAs = pool
		tlsCfg.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return tlsCfg, nil
}
