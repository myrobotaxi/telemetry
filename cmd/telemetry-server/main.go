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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/tnando/my-robo-taxi-telemetry/internal/auth"
	"github.com/tnando/my-robo-taxi-telemetry/internal/config"
	"github.com/tnando/my-robo-taxi-telemetry/internal/drives"
	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
	"github.com/tnando/my-robo-taxi-telemetry/internal/geocode"
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
	hub := ws.NewHub(logger.With(slog.String("component", "ws")), ws.NoopHubMetrics{})
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
	var authenticator ws.Authenticator
	if *devMode {
		logger.Warn("dev mode enabled: WebSocket auth disabled, accepting any token")
		authenticator = &ws.NoopAuthenticator{}
	} else {
		authenticator = auth.NewJWTAuthenticator(
			cfg.Auth().Secret,
			cfg.Auth().TokenIssuer,
			cfg.Auth().TokenAudience,
			db.Pool(),
		)
		logger.Info("JWT authentication enabled for WebSocket clients")
	}

	// --- HTTP servers ---
	srv := server.New(cfg.Server(), logger, db, reg, cfg.TeslaPublicKey())
	srv.SetTeslaHandler(recv.Handler())
	originPatterns := cfg.WebSocket().AllowedOrigins
	if len(originPatterns) == 0 {
		originPatterns = []string{"*"} // default: allow all (restrict in production config)
	}
	srv.SetClientHandler(hub.Handler(authenticator, ws.HandlerConfig{
		WriteTimeout:   cfg.WebSocket().WriteTimeout,
		OriginPatterns: originPatterns,
	}))

	// --- Vehicle status endpoint (always available) ---
	statusHandler := telemetry.NewVehicleStatusHandler(
		authenticator,
		&vehicleOwnerAdapter{cache: vinCache},
		recv,
		logger.With(slog.String("component", "vehicle-status")),
	)
	srv.HandleFunc("GET /api/vehicle-status/{vin}", statusHandler.ServeHTTP)

	// --- Fleet config push endpoint (optional — requires proxy config) ---
	setupFleetConfigEndpoint(cfg, srv, authenticator, vinCache, accountRepo, logger)

	// --- Debug fields endpoint ---
	// Mounted when resolveDebugFieldsGate says so — either because the
	// server is running with --dev (token optional) or because an operator
	// has set DEBUG_FIELDS_TOKEN on a production instance to let
	// `ops fields watch` stream real-Tesla frames. Auth is enforced by
	// DebugFieldsHandler via the X-Debug-Token header / ?token= query param
	// when APIKey is non-empty.
	if debugGate.Enabled {
		debugHandler := telemetry.NewDebugFieldsHandler(
			bus,
			logger.With(slog.String("component", "debug-fields")),
			telemetry.DebugFieldsConfig{
				APIKey:         debugGate.Token,
				OriginPatterns: originPatterns,
			},
		)
		srv.HandleFunc("GET /api/debug/fields", debugHandler.ServeHTTP)
		logger.Info("/api/debug/fields endpoint enabled",
			slog.String("gate", debugGate.Reason),
			slog.Bool("token_required", debugGate.Token != ""),
		)
	}

	// Configure mTLS on Tesla port. TLS is required for vehicle connections —
	// without it, Tesla vehicles cannot complete the handshake and report EOF.
	if cfg.TLS().CertFile == "" || cfg.TLS().KeyFile == "" {
		logger.Warn("TLS cert/key not configured — Tesla mTLS port will serve plain TCP (dev only)",
			slog.String("cert_file", cfg.TLS().CertFile),
			slog.String("key_file", cfg.TLS().KeyFile),
		)
	} else {
		teslaTLS, err := buildTeslaTLS(cfg.TLS())
		if err != nil {
			return fmt.Errorf("building Tesla mTLS config: %w", err)
		}
		srv.SetTeslaTLS(teslaTLS)
		logger.Info("Tesla mTLS configured",
			slog.String("cert_file", cfg.TLS().CertFile),
			slog.Bool("client_ca_loaded", cfg.TLS().CAFile != ""),
		)
	}

	logger.Info("starting HTTP servers")
	if err := srv.Start(ctx); err != nil {
		return fmt.Errorf("server: %w", err)
	}

	logger.Info("telemetry-server stopped cleanly")
	return nil
}

// setupFleetConfigEndpoint registers the POST /api/fleet-config/{vin}
// handler if the proxy URL and fleet telemetry hostname are configured.
// When Tesla OAuth credentials are available, it also enables automatic
// token refresh.
func setupFleetConfigEndpoint(
	cfg *config.Config,
	srv *server.Server,
	authenticator ws.Authenticator,
	vinCache *store.VINCache,
	accountRepo *store.AccountRepo,
	logger *slog.Logger,
) {
	if cfg.Proxy().URL == "" || cfg.Proxy().FleetTelemetryHostname == "" {
		logger.Warn("fleet config push disabled: proxy URL or telemetry hostname not configured")
		return
	}

	fleetClient := telemetry.NewFleetAPIClient(telemetry.FleetAPIConfig{
		BaseURL:    cfg.Proxy().URL,
		HTTPClient: proxyHTTPClient(cfg.Proxy().URL, logger),
	}, logger.With(slog.String("component", "fleet")))

	// Map config.ProxyConfig fields → telemetry.EndpointConfig.
	// If new proxy fields are added to config, update this mapping.
	var fleetOpts []telemetry.FleetConfigOption
	if cfg.TeslaOAuth().ClientID != "" {
		// Intentional mapping: config.TeslaOAuthConfig and telemetry.TeslaOAuthConfig
		// have identical fields but live in separate dependency layers. Don't "DRY"
		// them — config is infra, telemetry is domain. The copy keeps them decoupled.
		refresher := telemetry.NewTokenRefresher(telemetry.TeslaOAuthConfig{
			ClientID:     cfg.TeslaOAuth().ClientID,
			ClientSecret: cfg.TeslaOAuth().ClientSecret,
		}, logger.With(slog.String("component", "token-refresh")))
		updater := &teslaTokenUpdaterAdapter{repo: accountRepo}
		fleetOpts = append(fleetOpts, telemetry.WithTokenRefresher(refresher, updater))
		logger.Info("Tesla token auto-refresh enabled")
	} else {
		logger.Warn("Tesla token auto-refresh disabled: AUTH_TESLA_ID not set")
	}

	fleetHandler := telemetry.NewFleetConfigHandler(
		authenticator,
		&vehicleOwnerAdapter{cache: vinCache},
		&teslaTokenAdapter{repo: accountRepo},
		fleetClient,
		telemetry.EndpointConfig{
			Hostname: cfg.Proxy().FleetTelemetryHostname,
			Port:     cfg.Proxy().FleetTelemetryPort,
			CA:       cfg.Proxy().FleetTelemetryCA,
		},
		logger.With(slog.String("component", "fleet-config")),
		fleetOpts...,
	)

	srv.HandleFunc("POST /api/fleet-config/{vin}", fleetHandler.ServeHTTP)
	logger.Info("fleet config push endpoint enabled",
		slog.String("proxy_url", cfg.Proxy().URL),
	)
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
