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

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/drives"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/mask"
	"github.com/myrobotaxi/telemetry/internal/server"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
	"github.com/myrobotaxi/telemetry/internal/ws"
)

// accountTokenGaugeInterval is how often the running server polls the
// Account table for plaintext-without-ciphertext tokens. The rollout
// window is hours/days, not seconds — 1h cadence is plenty for an
// alerting pipeline and a 12× reduction in COUNT(*) scans vs. the
// prior 5m. MYR-131 flagged the 5m cadence as the bigger disk-IO
// offender on Nano compute.
const accountTokenGaugeInterval = 1 * time.Hour

// vehicleGPSGaugeInterval is the MYR-63 sibling of
// accountTokenGaugeInterval — same 1h cadence over the six Vehicle
// GPS *Enc columns. The two loops are independent so a stall in one
// (e.g., a long-running migration) doesn't starve the other.
const vehicleGPSGaugeInterval = 1 * time.Hour

// routeBlobGaugeInterval is the MYR-64 sibling — same 1h cadence over
// Vehicle.navRouteCoordinatesEnc and Drive.routePointsEnc. The
// route-blob queries can be heavier (jsonb columns), which is exactly
// why their old 5m cadence kept timing out under statement_timeout on
// Nano. 1h gives them headroom and matches the other two gauges.
const routeBlobGaugeInterval = 1 * time.Hour

// storePoolStatsInterval is how often the running server samples the
// pgxpool stats (acquired/idle/total) and pushes them into the
// telemetry_store_pool_*_conns gauges. 15s is short enough to catch
// short-lived contention spikes between scrapes (Prometheus default
// 60s) but long enough that the .Stat() call is invisible at the
// process level. MYR-138 wired this in after MYR-132 found the
// testbench had no pool visibility.
const storePoolStatsInterval = 15 * time.Second

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

func run() error { //nolint:funlen,cyclop // composition root — sequential dependency wiring; helpers extracted to wiring.go
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
	// MYR-62/63/64 wired the Encryptor into AccountRepo, VehicleRepo,
	// and DriveRepo for the dual-write rollouts of OAuth tokens, GPS
	// coordinates, and route-blob polylines respectively. MYR-65 wires
	// `cryptox_decrypt_total{version="N"}` against this registry so
	// operators can monitor v1-decay during a key rotation
	// (key-rotation.md §"Procedure" step 6).
	encryptor, err := setupEncryption(reg, logger)
	if err != nil {
		return err
	}

	// --- Store metrics (MYR-138) ---
	// Single PrometheusMetrics instance shared by NewDB, VehicleRepo,
	// and DriveRepo so all telemetry_store_* series come from one
	// coherent surface. Replaces the three NoopMetrics{} sites that
	// MYR-132's testbench audit found were hiding DB latency / errors
	// / pool stats from prod observability.
	storeMetrics := store.NewPrometheusMetrics(reg)

	// --- Database connection ---
	db, err := store.NewDB(ctx, cfg.Database(), logger.With(slog.String("component", "store")), storeMetrics)
	if err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer db.Close()

	// --- Pool-stats collector (MYR-138) ---
	// Periodically samples the pgxpool stats and publishes them via
	// storeMetrics.SetPoolStats so the gauges reflect live state
	// between Prometheus scrapes. Goroutine exits when ctx cancels.
	startPoolStatsCollector(ctx, db, storePoolStatsInterval, logger.With(slog.String("component", "store-pool-stats")))

	// --- Database migrations (Go-owned tables) ---
	// Applies all embedded SQL migrations in internal/store/migrations/ to the
	// _telemetry_* namespace. Prisma-owned tables are never touched here.
	// Fail-fast: a migration error indicates a broken schema that will cause
	// runtime failures -- there is no safe degraded mode.
	// See docs/architecture/migrations.md for the coexistence rule.
	if err := store.RunMigrations(ctx, cfg.Database().URL, logger.With(slog.String("component", "migrations"))); err != nil {
		return fmt.Errorf("running database migrations: %w", err)
	}

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

	// --- Store repos (needed by the Detector reconciler) ---
	// MYR-63 wires the Encryptor into VehicleRepo so the six GPS
	// columns are dual-written (plaintext + *Enc) and read with
	// ciphertext preference. Half-pair *Enc rows fall back to
	// plaintext per the atomic-pair invariant in
	// vehicle-state-schema.md §3.3.
	vehicleRepo := store.NewVehicleRepoWithEncryption(db.Pool(), storeMetrics, encryptor, logger.With(slog.String("component", "vehicle-repo")))
	driveRepo := store.NewDriveRepoWithEncryption(db.Pool(), storeMetrics, encryptor, logger.With(slog.String("component", "drive-repo")))
	accountRepo := store.NewAccountRepo(db.Pool(), encryptor)

	// MYR-173/174: ride-request repo. go_ride_requests stores pickup/dropoff
	// coordinates encrypt-only, so the Encryptor is mandatory (the
	// constructor panics on nil). Backs the P10 ride-hailing REST surface.
	rideRepo := store.NewRideRequestRepo(db.Pool(), storeMetrics, encryptor)

	// --- Drive detector ---
	// MYR-146: reconciler reads open Drive rows on Start so a Fly
	// redeploy mid-drive doesn't leave forever-active rows. The
	// adapter lives in adapters.go and translates store.OpenDriveRow
	// into the drives package's local type.
	detector := drives.NewDetector(
		bus,
		cfg.Drives(),
		logger.With(slog.String("component", "drives")),
		drives.NewPrometheusDetectorMetrics(reg),
		&openDriveListerAdapter{repo: driveRepo},
	)
	if err := detector.Start(ctx); err != nil {
		return fmt.Errorf("starting drive detector: %w", err)
	}
	defer func() { _ = detector.Stop() }()

	// MYR-62 + MYR-63 plaintext-zero gauges. Both register against the
	// same Prometheus registry the /metrics handler scrapes; each
	// refreshes on its own goroutine until the rollouts complete. See
	// startPlaintextGauges in wiring.go.
	startPlaintextGauges(ctx, reg, db.Pool(), accountTokenGaugeInterval, vehicleGPSGaugeInterval, routeBlobGaugeInterval, logger)

	// --- TLS endpoint cert monitor (MYR-188) ---
	// Probes the served leaf cert on each configured public endpoint so an
	// impending expiry pages BEFORE it takes down customer traffic —
	// including the Fly-terminated 4443 cert that no file monitor can see.
	startCertEndpointMonitor(ctx, reg, cfg.Monitoring().CertEndpoints, logger)

	// --- Audit sidecar (MYR-77) ---
	// Best-effort S3 mirror of every AuditLog INSERT.
	// No-op when AUDIT_SIDECAR_BUCKET is empty (local dev).
	// Production: set AUDIT_SIDECAR_BUCKET + AUDIT_SIDECAR_REGION; the service
	// IAM role (telemetry-server-audit-sidecar) grants s3:PutObject only —
	// see deployments/terraform/audit-sidecar/iam.tf.
	auditRepo, err := buildAuditRepo(ctx, reg, db.Pool(), logger)
	if err != nil {
		return fmt.Errorf("building audit repo: %w", err)
	}

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
		// MYR-137: unicast a per-atomic-group snapshot to a client on
		// subscribe so model/year/color/estimatedRange/fsdMilesSinceReset
		// (already persisted per MYR-24) reach the WS wire immediately
		// instead of only via the REST snapshot endpoint.
		ws.WithVehicleSnapshotReader(&wsVehicleSnapshotAdapter{repo: vehicleRepo}),
	)
	defer hub.Stop()

	// Shared VIN → (vehicleID, userID) cache backing the broadcaster and
	// the HTTP handlers below. Both identifiers are immutable for the
	// lifetime of a vehicle row, so the cache lives forever and a single
	// slim two-column query runs per VIN for the lifetime of the process.
	// This replaces ~660k full-row fetches per billing cycle that were
	// pulling the heavy navRouteCoordinates JSON on every telemetry frame.
	vinCache := store.NewVINCache(vehicleRepo, logger.With(slog.String("component", "vin-cache")))
	recv.SetAuthorizer(&vehicleAuthorizerAdapter{cache: vinCache})

	vinResolver := &vinResolverAdapter{cache: vinCache}
	broadcaster := ws.NewBroadcaster(hub, bus, vinResolver, logger.With(slog.String("component", "broadcaster")))
	if err := broadcaster.Start(ctx); err != nil {
		return fmt.Errorf("starting broadcaster: %w", err)
	}
	defer func() { _ = broadcaster.Stop() }()

	go hub.RunHeartbeat(ctx, cfg.WebSocket().HeartbeatInterval)

	// --- Identity module keystore (MYR-193, ADR-001) ---
	// The ES256 signing keystore backs both access-token minting and the
	// dual-alg validator's ES256 verification. Nil => module disabled
	// (HS256-only). Built before the authenticator so its public-key resolver
	// can be injected into JWT validation.
	keystore, err := buildKeystore(cfg, *devMode, logger)
	if err != nil {
		return fmt.Errorf("building identity keystore: %w", err)
	}
	var es256Resolver auth.ES256KeyResolver
	if keystore != nil {
		es256Resolver = keystore
	}

	// --- Client authenticator ---
	authenticator := setupAuthenticator(cfg, db.Pool(), *devMode, es256Resolver, logger)

	// --- vehicle_deleted cleanup pipeline (FR-10.1 / data-lifecycle.md §3.5, MYR-73) ---
	// Postgres LISTEN/NOTIFY goroutine + dispatcher that fans the event
	// out to the WS hub, the Tesla receiver, the VIN cache, and the JWT
	// user-existence cache. Production wires a real JWTAuthenticator;
	// dev mode uses NoopAuthenticator (no user cache to invalidate).
	jwtAuth, _ := authenticator.(*auth.JWTAuthenticator)
	dispatcher := newVehicleDeletedDispatcher(hub, recv, vinCache, jwtAuth, logger.With(slog.String("component", "vehicle-deleted-dispatcher")))
	if _, err := dispatcher.Subscribe(bus); err != nil {
		return fmt.Errorf("subscribe vehicle_deleted dispatcher: %w", err)
	}
	runNotifyListener(ctx, cfg.Database().URL, bus, logger)

	// --- Nav-dispatch (MYR-176) ---
	// Subscribes to the ride.accepted seam (published by the owner-accept
	// handler) and pushes the rider's pickup into the vehicle's Tesla
	// navigation via the command Executor. Gated by DISPATCH_ENABLED.
	if err := setupNavDispatcher(ctx, cfg, bus, vehicleRepo, accountRepo, rideRepo, logger); err != nil {
		return fmt.Errorf("setting up nav dispatcher: %w", err)
	}

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
		vehicleRepo:    vehicleRepo,
		driveRepo:      driveRepo,
		rideRepo:       rideRepo,
		accountRepo:    accountRepo,
		pool:           db.Pool(),
		encryptor:      encryptor,
		auditEmitter:   auditEmitter,
		auditMetrics:   auditMetrics,
		debugGate:      debugGate,
		originPatterns: originPatterns,
		logger:         logger,
	})

	// --- Identity module endpoints (MYR-193, ADR-001) ---
	// Native Sign in with Apple, ES256 access-token minting, refresh
	// rotation, and the public JWKS at /api/auth/.well-known/jwks.json.
	// No-op when the keystore is disabled.
	setupIdentityEndpoints(cfg, srv, keystore, db.Pool(), logger)

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
