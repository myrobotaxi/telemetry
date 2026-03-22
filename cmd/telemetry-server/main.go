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

	"github.com/tnando/my-robo-taxi-telemetry/internal/auth"
	"github.com/tnando/my-robo-taxi-telemetry/internal/config"
	"github.com/tnando/my-robo-taxi-telemetry/internal/drives"
	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
	"github.com/tnando/my-robo-taxi-telemetry/internal/server"
	"github.com/tnando/my-robo-taxi-telemetry/internal/store"
	"github.com/tnando/my-robo-taxi-telemetry/internal/telemetry"
	"github.com/tnando/my-robo-taxi-telemetry/internal/ws"
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

func run() error { //nolint:funlen // composition root — sequential dependency wiring
	// --- Flag parsing ---
	var (
		configPath = flag.String("config", "", "path to JSON configuration file")
		logLevel   = flag.String("log-level", "info", "log level: debug, info, warn, error")
		devMode    = flag.Bool("dev", false, "dev mode: skip JWT auth, accept any token")
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

	// --- Drive detector ---
	detector := drives.NewDetector(bus, cfg.Drives(), logger.With(slog.String("component", "drives")), drives.NoopDetectorMetrics{})
	if err := detector.Start(ctx); err != nil {
		return fmt.Errorf("starting drive detector: %w", err)
	}
	defer func() { _ = detector.Stop() }()

	// --- Store repos ---
	vehicleRepo := store.NewVehicleRepo(db.Pool(), store.NoopMetrics{})
	driveRepo := store.NewDriveRepo(db.Pool(), store.NoopMetrics{})

	// --- Persistence writer ---
	writer := store.NewWriter(
		vehicleRepo, driveRepo, vehicleRepo, bus,
		logger.With(slog.String("component", "writer")),
		store.WriterConfig{
			FlushInterval: cfg.Telemetry().BatchWriteInterval,
			BatchSize:     cfg.Telemetry().BatchWriteSize,
		},
	)
	if err := writer.Start(ctx); err != nil {
		return fmt.Errorf("starting persistence writer: %w", err)
	}
	defer func() { _ = writer.Stop() }()

	// --- WebSocket hub + broadcaster ---
	hub := ws.NewHub(logger.With(slog.String("component", "ws")), ws.NoopHubMetrics{})
	defer hub.Stop()

	vinResolver := &vinResolverAdapter{repo: vehicleRepo}
	broadcaster := ws.NewBroadcaster(hub, bus, vinResolver, logger.With(slog.String("component", "broadcaster")))
	if err := broadcaster.Start(ctx); err != nil {
		return fmt.Errorf("starting broadcaster: %w", err)
	}
	defer func() { _ = broadcaster.Stop() }()

	go hub.RunHeartbeat(ctx, cfg.WebSocket().HeartbeatInterval)

	// --- Client authenticator ---
	var authenticator ws.Authenticator
	if *devMode {
		logger.Warn("dev mode enabled: WebSocket auth disabled, accepting any token")
		authenticator = &ws.NoopAuthenticator{}
	} else {
		authenticator = auth.NewJWTAuthenticator(cfg.Auth().Secret, db.Pool())
		logger.Info("JWT authentication enabled for WebSocket clients")
	}

	// --- HTTP servers ---
	srv := server.New(cfg.Server(), logger, db, reg)
	srv.SetTeslaHandler(recv.Handler())
	srv.SetClientHandler(hub.Handler(authenticator, ws.HandlerConfig{
		WriteTimeout:   cfg.WebSocket().WriteTimeout,
		OriginPatterns: []string{"*"}, // TODO: restrict to frontend origin in production
	}))

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

// vinResolverAdapter adapts store.VehicleRepo (returns Vehicle) to the
// ws.VINResolver interface (returns vehicleID string).
type vinResolverAdapter struct {
	repo *store.VehicleRepo
}

func (a *vinResolverAdapter) GetByVIN(ctx context.Context, vin string) (string, error) {
	v, err := a.repo.GetByVIN(ctx, vin)
	if err != nil {
		return "", fmt.Errorf("resolve VIN: %w", err)
	}
	return v.ID, nil
}
