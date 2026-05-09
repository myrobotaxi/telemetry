// Binary telemetry-server receives real-time vehicle telemetry from Tesla's
// Fleet Telemetry system and broadcasts it to connected browser clients via
// WebSocket. This file is the composition root — it wires dependencies and
// starts the server. No business logic lives here.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/tnando/my-robo-taxi-telemetry/internal/config"
	"github.com/tnando/my-robo-taxi-telemetry/internal/drives"
	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
	"github.com/tnando/my-robo-taxi-telemetry/internal/geocode"
	"github.com/tnando/my-robo-taxi-telemetry/internal/mask"
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

func run() error { //nolint:funlen // composition root — sequential dependency wiring; helpers extracted to wiring.go
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

	// --- Debug-fields gate ---
	// Either --dev or a non-empty DEBUG_FIELDS_TOKEN turns on the
	// RawVehicleTelemetryEvent pipeline and mounts /api/debug/fields.
	// In non-dev mode the token must be at least 32 chars so `ops fields
	// watch` can stream real-Tesla data against production behind a
	// real secret.
	debugGate, err := resolveDebugFieldsGate(*devMode, os.Getenv("DEBUG_FIELDS_TOKEN"))
	if err != nil {
		return fmt.Errorf("invalid debug-fields configuration: %w", err)
	}

	// --- Prometheus registry ---
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	// --- Column encryption foundation (NFR-3.23, NFR-3.24) ---
	encryptor, err := setupEncryption(logger)
	if err != nil {
		return err
	}
	_ = encryptor // foundation: column wiring lands in follow-on PRs (MYR-16 cross-repo rollout issues)

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
			// Raw field publication feeds /api/debug/fields. Enabled
			// whenever the debug-fields gate is open (dev mode OR
			// DEBUG_FIELDS_TOKEN set) so operators can tail real-Tesla
			// frames against production without extra deploys.
			PublishRawFields: debugGate.Enabled,
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
	accountRepo := store.NewAccountRepo(db.Pool())
	auditRepo := store.NewAuditRepo(db.Pool())

	// --- Mask-audit emitter (MYR-71, rest-api.md §5.3) ---
	// MaskAuditEmitter adapts AuditRepo to the mask.AuditEmitter
	// interface so the hub and any future REST mask paths can fire
	// non-blocking audit rows. Prometheus counters
	// telemetry_audit_log_writes_total{action,target} and
	// telemetry_audit_log_write_failures_total{action,target} make
	// the 1% sample rate observable in prod.
	auditEmitter := store.NewMaskAuditEmitter(auditRepo)
	auditMetrics := mask.NewPrometheusAuditMetrics(reg)

	// --- Geocoder (optional — requires MAPBOX_TOKEN) ---
	geo := newGeocoder(cfg.MapboxToken(), cfg.Drives().GeocodeTimeout, logger)

	// --- Persistence writer ---
	writer := store.NewWriter(
		vehicleRepo, driveRepo, vehicleRepo, bus, geo,
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
	// WithMaskAudit wires the per-(vehicleID, role, frame) mask audit
	// emit at the hub layer per rest-api.md §5.3. The hub itself uses
	// a per-vehicle in-process counter as frameSeq until DV-02 ships
	// envelope sequence numbers.
	hub := ws.NewHub(
		logger.With(slog.String("component", "ws")),
		ws.NoopHubMetrics{},
		ws.WithMaskAudit(auditEmitter, auditMetrics),
	)
	defer hub.Stop()

	// Shared VIN → (vehicleID, userID) cache backing the broadcaster and
	// the HTTP handlers below. Both identifiers are immutable for the
	// lifetime of a vehicle row, so the cache lives forever and a single
	// slim two-column query runs per VIN for the lifetime of the process.
	// This replaces ~660k full-row fetches per billing cycle that were
	// pulling the heavy navRouteCoordinates JSON on every telemetry frame.
	vinCache := store.NewVINCache(vehicleRepo, logger.With(slog.String("component", "vin-cache")))

	vinResolver := &vinResolverAdapter{cache: vinCache}
	broadcaster := ws.NewBroadcaster(hub, bus, vinResolver, logger.With(slog.String("component", "broadcaster")))
	if err := broadcaster.Start(ctx); err != nil {
		return fmt.Errorf("starting broadcaster: %w", err)
	}
	defer func() { _ = broadcaster.Stop() }()

	go hub.RunHeartbeat(ctx, cfg.WebSocket().HeartbeatInterval)

	// --- Client authenticator ---
	authenticator := setupAuthenticator(cfg, db.Pool(), *devMode, logger)

	// --- HTTP server + route registration ---
	srv := server.New(cfg.Server(), logger, db, reg, cfg.TeslaPublicKey())
	originPatterns := resolveWSOriginPatterns(cfg.WebSocket().AllowedOrigins, *devMode, logger)
	setupHTTPHandlers(httpRouteDeps{
		cfg:            cfg,
		srv:            srv,
		hub:            hub,
		authenticator:  authenticator,
		recv:           recv,
		bus:            bus,
		vinCache:       vinCache,
		accountRepo:    accountRepo,
		debugGate:      debugGate,
		originPatterns: originPatterns,
		logger:         logger,
	})

	// --- Tesla mTLS ---
	if err := setupTeslaTLS(cfg, srv, logger); err != nil {
		return err
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

// newGeocoder creates a Geocoder based on whether a Mapbox token is
// available. Returns NoopGeocoder when the token is empty.
func newGeocoder(token string, timeout time.Duration, logger *slog.Logger) geocode.Geocoder {
	if g := geocode.NewMapboxGeocoder(token, timeout); g != nil {
		logger.Info("Mapbox reverse geocoding enabled for drive addresses")
		return g
	}
	logger.Warn("Mapbox token not set — drive addresses will show raw coordinates")
	return geocode.NoopGeocoder{}
}
