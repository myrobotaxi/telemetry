// Wiring helpers split out of main.go to keep the composition root under
// the CLAUDE.md 300-line cap. None of these add abstraction over what
// run() already did inline — they are pure code-organization extractions.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/myrobotaxi/telemetry/internal/auth"
	"github.com/myrobotaxi/telemetry/internal/commands"
	"github.com/myrobotaxi/telemetry/internal/config"
	"github.com/myrobotaxi/telemetry/internal/cryptox"
	"github.com/myrobotaxi/telemetry/internal/events"
	"github.com/myrobotaxi/telemetry/internal/geocode"
	"github.com/myrobotaxi/telemetry/internal/mask"
	"github.com/myrobotaxi/telemetry/internal/server"
	"github.com/myrobotaxi/telemetry/internal/store"
	"github.com/myrobotaxi/telemetry/internal/store/accountbackfill"
	"github.com/myrobotaxi/telemetry/internal/store/routeblobbackfill"
	"github.com/myrobotaxi/telemetry/internal/store/vehiclegpsbackfill"
	"github.com/myrobotaxi/telemetry/internal/telemetry"
	"github.com/myrobotaxi/telemetry/internal/ws"
)

// newLogger constructs the structured logger the binary uses for the
// rest of its lifetime. JSON in prod (LOG_FORMAT=json), text otherwise.
func newLogger(level string) (*slog.Logger, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("parsing log level %q: %w", level, err)
	}
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

// setupEncryption loads the AES-256-GCM key set (fails fast on missing
// or invalid ENCRYPTION_KEY per NFR-3.23/NFR-3.24) and registers the
// per-version `cryptox_decrypt_total` counter on reg with one zero-
// valued series per readable key version, so /metrics shows the full
// label set on the first scrape. Operators read it during rotation to
// confirm v1-decay (key-rotation.md procedure step 6).
func setupEncryption(reg prometheus.Registerer, logger *slog.Logger) (cryptox.Encryptor, error) {
	keySet, err := cryptox.LoadKeySetFromEnv()
	if err != nil {
		return nil, fmt.Errorf("loading encryption key set: %w", err)
	}
	encryptor, err := cryptox.NewEncryptor(keySet,
		cryptox.WithMetrics(cryptox.NewPrometheusMetrics(reg, keySet.ReadableVersions())))
	if err != nil {
		return nil, fmt.Errorf("constructing encryptor: %w", err)
	}
	logger.Info("encryptor initialized",
		slog.Int("write_version", int(keySet.WriteVersion())),
		slog.Int("readable_versions", len(keySet.ReadableVersions())))
	return encryptor, nil
}

// startPlaintextGauges registers and runs the three cross-repo
// encryption rollout health gauges:
//   - account_token_plaintext_remaining_total (MYR-62)
//   - vehicle_gps_plaintext_remaining_total   (MYR-63)
//   - route_blob_plaintext_remaining_total    (MYR-64)
//
// Each loop runs on a background goroutine tied to ctx and refreshes
// on its own cadence so a stall in one doesn't starve the others.
func startPlaintextGauges(
	ctx context.Context,
	reg prometheus.Registerer,
	pool *pgxpool.Pool,
	accountInterval, gpsInterval, routeBlobInterval time.Duration,
	logger *slog.Logger,
) {
	accountGauge := accountbackfill.NewPlaintextGauge(reg, pool, accountInterval, logger.With(slog.String("component", "account-token-gauge")))
	go accountGauge.Run(ctx)

	gpsGauge := vehiclegpsbackfill.NewPlaintextGauge(reg, pool, gpsInterval, logger.With(slog.String("component", "vehicle-gps-gauge")))
	go gpsGauge.Run(ctx)

	routeBlobGauge := routeblobbackfill.NewPlaintextGauge(reg, pool, routeBlobInterval, logger.With(slog.String("component", "route-blob-gauge")))
	go routeBlobGauge.Run(ctx)
}

// startCertEndpointMonitor wires the endpoint TLS certificate monitor,
// which TLS-dials each configured host:port and exposes the served leaf's
// expiry as Prometheus gauges. Unlike the file-based monitor it sees certs
// terminated outside this process (notably the Fly-managed cert on the
// client WebSocket port) — the blind spot behind the MYR-188 outage. When
// no endpoints are configured (TLS_MONITOR_ENDPOINTS unset) it is a no-op.
func startCertEndpointMonitor(ctx context.Context, reg prometheus.Registerer, endpoints []string, logger *slog.Logger) {
	if len(endpoints) == 0 {
		logger.Info("tls endpoint cert monitor disabled (set TLS_MONITOR_ENDPOINTS to enable)")
		return
	}
	monitor := telemetry.NewEndpointCertMonitor(telemetry.EndpointCertMonitorConfig{
		Endpoints: endpoints,
	}, reg, logger.With(slog.String("component", "cert-endpoint-monitor")))
	go monitor.Run(ctx)
	logger.Info("tls endpoint cert monitor started", slog.Any("endpoints", endpoints))
}

// startPoolStatsCollector spawns a background goroutine that polls
// db.CollectPoolStats every interval and exits when ctx cancels.
// CollectPoolStats reads pgxpool.Stat() and pushes the values into the
// store.Metrics implementation wired into the DB — under MYR-138 that
// is store.PrometheusMetrics, so the gauges land on /metrics. The
// .Stat() call is a cheap accessor over atomic counters, so the 15s
// cadence is well below any measurable cost.
func startPoolStatsCollector(ctx context.Context, db *store.DB, interval time.Duration, logger *slog.Logger) {
	logger.Info("store pool-stats collector started", slog.Duration("interval", interval))
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		// Sample once immediately so the gauges aren't NaN/zero for
		// the first interval after startup.
		db.CollectPoolStats()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				db.CollectPoolStats()
			}
		}
	}()
}

// setupAuthenticator returns a NoopAuthenticator in dev mode (accepts any
// token) or a JWTAuthenticator wired against the auth secret + DB pool in
// production mode. When es256 is non-nil the validator additionally accepts
// ES256 tokens minted by the identity module (ADR-001 §3, dual-alg); legacy
// HS256 acceptance is unchanged either way.
func setupAuthenticator(cfg *config.Config, dbPool *pgxpool.Pool, devMode bool, es256 auth.ES256KeyResolver, logger *slog.Logger) ws.Authenticator {
	if devMode {
		logger.Warn("dev mode enabled: WebSocket auth disabled, accepting any token")
		return &ws.NoopAuthenticator{}
	}
	var opts []auth.Option
	if es256 != nil {
		opts = append(opts, auth.WithES256Resolver(es256))
		logger.Info("ES256 access-token validation enabled (identity module keystore)")
	}
	logger.Info("JWT authentication enabled for WebSocket clients")
	return auth.NewJWTAuthenticator(
		cfg.Auth().Secret,
		cfg.Auth().TokenIssuer,
		cfg.Auth().TokenAudience,
		dbPool,
		opts...,
	)
}

// httpRouteDeps bundles the dependencies required to register the HTTP
// route surface. Grouped into a struct so setupHTTPHandlers's signature
// stays readable and so adding a new dep doesn't ripple through call
// sites.
type httpRouteDeps struct {
	cfg           *config.Config
	srv           *server.Server
	hub           *ws.Hub
	authenticator ws.Authenticator
	recv          *telemetry.Receiver
	bus           events.Bus
	vinCache      *store.VINCache
	vehicleRepo   *store.VehicleRepo
	driveRepo     *store.DriveRepo
	rideRepo      *store.RideRequestRepo
	accountRepo   *store.AccountRepo
	// pushRepo backs the MYR-186 device-registry endpoints.
	pushRepo *store.PushDeviceRepo
	// shareRepo backs the MYR-184 vehicle-sharing endpoints.
	shareRepo *store.VehicleShareRepo
	// accessInvalidator busts a user's cached vehicle access set when a
	// share is redeemed or revoked (MYR-184). Satisfied by the
	// authenticator; separate from it so the sharing handlers depend on the
	// one-method interface rather than the whole authenticator.
	accessInvalidator telemetry.AccessCacheInvalidator
	pool              *pgxpool.Pool
	encryptor         cryptox.Encryptor
	auditEmitter      mask.AuditEmitter
	auditMetrics      mask.AuditMetrics
	debugGate         debugFieldsGate
	originPatterns    []string
	// serviceStatus is the running in-service monitor. It also owns the
	// per-VIN stream-recency state (MYR-300) and the vehicle_data backfill
	// mapping (MYR-260) that the MYR-315 refresh endpoint reuses.
	serviceStatus *telemetry.ServiceStatusMonitor
	logger        *slog.Logger
}

// setupHTTPHandlers wires every HTTP handler the server exposes:
// the WebSocket client handler, the Tesla mTLS handler, the
// vehicle-status REST endpoint, the optional fleet-config push, and
// the optional debug-fields stream. It does NOT start the server —
// the caller owns srv.Start.
func setupHTTPHandlers(deps httpRouteDeps) {
	deps.srv.SetTeslaHandler(deps.recv.Handler())
	deps.srv.SetClientHandler(deps.hub.Handler(deps.authenticator, ws.HandlerConfig{
		WriteTimeout:   deps.cfg.WebSocket().WriteTimeout,
		OriginPatterns: deps.originPatterns,
	}))

	statusHandler := telemetry.NewVehicleStatusHandler(
		deps.authenticator,
		&vehicleOwnerAdapter{cache: deps.vinCache},
		deps.recv,
		deps.logger.With(slog.String("component", "vehicle-status")),
	)
	deps.srv.HandleFunc("GET /api/vehicle-status/{vin}", statusHandler.ServeHTTP)

	// MYR-91: GET /api/vehicles — list endpoint that enumerates the
	// caller's vehicles for SDK consumers (rest-api.md §7.0). v1
	// returns owned vehicles only; the viewer-merged pathway is
	// PLANNED.
	//
	// MYR-122: the adapter binds to the LEAN read path
	// (VehicleRepo.ListSummariesByUser) — list endpoints MUST NOT
	// SELECT columns the response body doesn't emit (AGENTS.md
	// "Performance invariants"). Wide reads belong only on detail/edit
	// handlers.
	vehiclesListHandler := telemetry.NewVehiclesListHandler(
		deps.authenticator,
		&vehicleListerAdapter{repo: deps.vehicleRepo},
		deps.logger.With(slog.String("component", "vehicles-list")),
		// MYR-184 / MYR-91 viewer merge: shared vehicles are appended as
		// `role: "viewer"` rows carrying their sharePermission.
		telemetry.WithSharedVehicles(&sharedVehicleListerAdapter{repo: deps.vehicleRepo}),
	)
	deps.srv.HandleFunc("GET /api/vehicles", vehiclesListHandler.ServeHTTP)

	// MYR-133: GET /api/vehicles/{vehicleId}/snapshot and
	// /drives — REST endpoints that close the DV-20 PENDING gap on
	// the Go server. Both mirror the rest-api.md §7.1 / §7.2 contract:
	// bearer-token validation, vehicleId path param (cuid, NOT VIN),
	// ownership verification via VehicleRepo.GetByID, role-based field
	// mask projection at the handler layer per rest-api.md §5.1.
	snapshotAdapter := &vehicleSnapshotAdapter{repo: deps.vehicleRepo}
	snapshotHandler := telemetry.NewVehicleSnapshotHandler(
		deps.authenticator,
		snapshotAdapter,
		deps.logger.With(slog.String("component", "vehicle-snapshot")),
		telemetry.WithSnapshotRoleResolver(deps.authenticator),
		telemetry.WithSnapshotShareReader(&shareReaderAdapter{repo: deps.shareRepo}),
		telemetry.WithSnapshotMaskAudit(deps.auditEmitter, deps.auditMetrics, "/api/vehicles/{vehicleId}/snapshot"),
	)
	deps.srv.HandleFunc("GET /api/vehicles/{vehicleId}/snapshot", snapshotHandler.ServeHTTP)

	drivesHandler := telemetry.NewVehicleDrivesHandler(
		deps.authenticator,
		snapshotAdapter,
		&driveListerAdapter{repo: deps.driveRepo},
		deps.logger.With(slog.String("component", "vehicle-drives")),
		telemetry.WithDrivesRoleResolver(deps.authenticator),
		telemetry.WithDrivesShareReader(&shareReaderAdapter{repo: deps.shareRepo}),
		telemetry.WithDrivesMaskAudit(deps.auditEmitter, deps.auditMetrics, "/api/vehicles/{vehicleId}/drives"),
	)
	deps.srv.HandleFunc("GET /api/vehicles/{vehicleId}/drives", drivesHandler.ServeHTTP)

	setupDriveReadEndpoints(deps, snapshotAdapter)

	setupRideRequestEndpoints(deps, snapshotAdapter)

	setupFleetConfigEndpoint(deps.cfg, deps.srv, deps.authenticator, deps.vinCache, deps.accountRepo, deps.vehicleRepo, deps.logger)

	setupTeslaLinkEndpoints(deps.cfg, deps.srv, deps.authenticator, deps.pool, deps.encryptor, deps.logger)

	// Per-feature endpoint groups, each in its own wiring_*.go.
	setupVehicleTeardownEndpoint(deps)
	setupVehicleReaddEndpoint(deps)
	setupVehiclePlateEndpoint(deps)
	setupVehicleRefreshEndpoint(deps)
	setupVehicleServiceWindowEndpoint(deps)
	setupVehicleCommandEndpoint(deps, snapshotAdapter)
	setupPushDeviceEndpoints(deps)
	setupVehicleSharingEndpoints(deps, snapshotAdapter)
	setupDebugFieldsEndpoint(deps)
}

// setupDebugFieldsEndpoint mounts GET /api/debug/fields when
// resolveDebugFieldsGate says so — either because the server is running
// with --dev (token optional) or because an operator has set
// DEBUG_FIELDS_TOKEN on a production instance to let `ops fields watch`
// stream real-Tesla frames. Auth is enforced by DebugFieldsHandler via the
// X-Debug-Token header / ?token= query param when APIKey is non-empty.
func setupDebugFieldsEndpoint(deps httpRouteDeps) {
	if !deps.debugGate.Enabled {
		return
	}
	debugHandler := telemetry.NewDebugFieldsHandler(
		deps.bus,
		deps.logger.With(slog.String("component", "debug-fields")),
		telemetry.DebugFieldsConfig{
			APIKey:         deps.debugGate.Token,
			OriginPatterns: deps.originPatterns,
		},
	)
	deps.srv.HandleFunc("GET /api/debug/fields", debugHandler.ServeHTTP)
	deps.logger.Info("/api/debug/fields endpoint enabled",
		slog.String("gate", deps.debugGate.Reason),
		slog.Bool("token_required", deps.debugGate.Token != ""),
	)
}

// setupRideRequestEndpoints wires the ride-request REST surface: the
// rider-facing endpoints (P10 ride-hailing, MYR-174) plus the owner-facing
// incoming feed + accept/decline (MYR-175). The store adapter binds
// store.RideRequestRepo (the pickup/dropoff GPS encrypt/decrypt boundary);
// the bus is the RideEventPublisher — the WS broadcaster turns the published
// created/status-changed events into summary frames unicast to the two
// parties (rider + owner). The vehicles reader supplies the create-time
// vehicle access check + ownerId derivation.
func setupRideRequestEndpoints(deps httpRouteDeps, vehicles telemetry.VehicleSnapshotReader) {
	rideHandler := telemetry.NewRideRequestHandler(
		deps.authenticator,
		vehicles,
		&rideRequestStoreAdapter{repo: deps.rideRepo},
		deps.bus,
		deps.logger.With(slog.String("component", "ride-request")),
		// MYR-184: a rider holding a `rides` share may request a ride on a
		// car they do not own. This is the SEPARATE code path the read-side
		// access-set widening does not cover.
		telemetry.WithRideShareReader(&shareReaderAdapter{repo: deps.shareRepo}),
	)
	deps.srv.HandleFunc("POST /api/ride-requests", rideHandler.ServeCreate)
	deps.srv.HandleFunc("GET /api/ride-requests", rideHandler.ServeList)
	deps.srv.HandleFunc("GET /api/ride-requests/{id}", rideHandler.ServeGet)
	deps.srv.HandleFunc("POST /api/ride-requests/{id}/cancel", rideHandler.ServeCancel)

	// MYR-270: owner-driven dispatch v2 progress endpoints (supersedes the
	// MYR-265 /board endpoint). The owner confirms pickup (accepted→arrived) and
	// dropoff (enroute→completed); the RIDER starts the ride (arrived→enroute),
	// and the rider start is what publishes the ride.started seam the nav
	// dispatcher subscribes to for the DROPOFF nav push. Each is guarded and
	// idempotent (a repeat of the destination state is a 200 no-op).
	deps.srv.HandleFunc("POST /api/ride-requests/{id}/picked-up", rideHandler.ServePickedUp)
	deps.srv.HandleFunc("POST /api/ride-requests/{id}/start", rideHandler.ServeStart)
	deps.srv.HandleFunc("POST /api/ride-requests/{id}/dropped-off", rideHandler.ServeDroppedOff)

	// MYR-175: owner-facing surface. The literal /incoming segment takes
	// precedence over the {id} wildcard in Go's ServeMux, so both routes
	// coexist. Accept additionally publishes the ride.accepted dispatch
	// seam MYR-176 subscribes to for the Tesla navigation_request push.
	deps.srv.HandleFunc("GET /api/ride-requests/incoming", rideHandler.ServeIncoming)
	deps.srv.HandleFunc("POST /api/ride-requests/{id}/accept", rideHandler.ServeAccept)
	deps.srv.HandleFunc("POST /api/ride-requests/{id}/decline", rideHandler.ServeDecline)
}

// setupTeslaTLS configures mTLS on the Tesla port. Without it, Tesla
// vehicles cannot complete the handshake and report EOF. If the cert/key
// is not configured (dev only), the function logs a warning and returns
// nil so the Tesla port serves plain TCP.
func setupTeslaTLS(cfg *config.Config, srv *server.Server, logger *slog.Logger) error {
	if cfg.TLS().CertFile == "" || cfg.TLS().KeyFile == "" {
		logger.Warn("TLS cert/key not configured — Tesla mTLS port will serve plain TCP (dev only)",
			slog.String("cert_file", cfg.TLS().CertFile),
			slog.String("key_file", cfg.TLS().KeyFile),
		)
		return nil
	}
	teslaTLS, err := buildTeslaTLS(cfg.TLS())
	if err != nil {
		return fmt.Errorf("building Tesla mTLS config: %w", err)
	}
	srv.SetTeslaTLS(teslaTLS)
	logger.Info("Tesla mTLS configured",
		slog.String("cert_file", cfg.TLS().CertFile),
		slog.Bool("client_ca_loaded", cfg.TLS().CAFile != ""),
	)
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
	vehicleRepo *store.VehicleRepo,
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
	srv.HandleFunc("GET /api/fleet-config/{vin}", fleetHandler.ServeHTTP)

	// vehicleId-keyed variant for browser clients, which never receive a
	// full VIN (P0-masked). Resolves vehicleId→VIN server-side.
	vehicleFleetHandler := telemetry.NewVehicleFleetConfigHandler(
		fleetHandler,
		&vehicleSnapshotAdapter{repo: vehicleRepo},
		logger.With(slog.String("component", "vehicle-fleet-config")),
	)
	srv.HandleFunc("GET /api/fleet-config/vehicle/{vehicleId}", vehicleFleetHandler.ServeHTTP)
	srv.HandleFunc("POST /api/fleet-config/vehicle/{vehicleId}", vehicleFleetHandler.ServeHTTP)

	logger.Info("fleet config endpoints enabled (VIN + vehicleId, GET status + POST re-push)",
		slog.String("proxy_url", cfg.Proxy().URL),
	)
}

// setupVehicleCommandEndpoint mounts POST /api/vehicles/{vehicleId}/command/{name}
// (MYR-180). The signed-command transport reuses the tesla-http-proxy
// sidecar (cfg.Proxy().URL) that fleet-config push already uses — the
// P-256 command key lives ONLY in that proxy's config, never in this
// process (decision record: docs/operations/vehicle-commands.md §1). The
// route is ALWAYS mounted so the SDK sees a typed error, not a 404: when
// no proxy is configured the transport is disabled and every signer-
// required command resolves to key_not_paired.
func setupVehicleCommandEndpoint(deps httpRouteDeps, vehicles telemetry.VehicleSnapshotReader) {
	proxyURL := deps.cfg.Proxy().URL
	transport := newCommandTransport(proxyURL, deps.cfg.Proxy().FleetAPIBaseURL,
		deps.logger.With(slog.String("component", "command-transport")))
	executor := commands.NewExecutor(transport, deps.logger.With(slog.String("component", "command-executor")))

	var opts []telemetry.VehicleCommandOption
	if deps.cfg.TeslaOAuth().ClientID != "" {
		refresher := telemetry.NewTokenRefresher(telemetry.TeslaOAuthConfig{
			ClientID:     deps.cfg.TeslaOAuth().ClientID,
			ClientSecret: deps.cfg.TeslaOAuth().ClientSecret,
		}, deps.logger.With(slog.String("component", "command-token-refresh")))
		opts = append(opts, telemetry.WithCommandTokenRefresher(refresher, &teslaTokenUpdaterAdapter{repo: deps.accountRepo}))
	}

	handler := telemetry.NewVehicleCommandHandler(
		deps.authenticator,
		vehicles,
		&teslaTokenAdapter{repo: deps.accountRepo},
		executor,
		deps.logger.With(slog.String("component", "vehicle-command")),
		opts...,
	)
	deps.srv.HandleFunc("POST /api/vehicles/{vehicleId}/command/{name}", handler.ServeHTTP)

	if transport.Enabled() {
		deps.logger.Info("vehicle command endpoint enabled",
			slog.String("proxy_url", proxyURL),
			slog.Int("commands", len(executor.Registry().Names())),
		)
	} else {
		deps.logger.Warn("vehicle command endpoint mounted but signing disabled: TESLA_PROXY_URL not set — signer-required commands return key_not_paired")
	}
}

// newCommandTransport builds the RoutingTransport the command Executor uses:
// SIGNED commands go to the tesla-http-proxy (loopback-aware client, as
// before); UNSIGNED commands (navigation_request) go directly to the Fleet
// REST API because proxy v0.4.1 mis-forwards them (MYR-245). The Fleet REST
// transport uses a default, TLS-verified client — never the proxy's loopback
// InsecureSkipVerify client — because it dials the real public Fleet API host.
// fleetBaseURL is validated (https, non-empty) fail-fast in config.
func newCommandTransport(proxyURL, fleetBaseURL string, logger *slog.Logger) *commands.RoutingTransport {
	proxyTr := commands.NewProxyTransport(proxyURL, proxyHTTPClient(proxyURL, logger), logger)
	fleetTr := commands.NewFleetRESTTransport(fleetBaseURL, nil, logger)
	return commands.NewRoutingTransport(proxyTr, fleetTr, logger)
}

// buildTeslaTLS creates a TLS config for the Tesla mTLS port. It loads
// the server cert/key and optionally a CA for verifying client certs.
// If no CA file is configured, client certs are requested but not
// verified (suitable for local dev with self-signed certs).
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
