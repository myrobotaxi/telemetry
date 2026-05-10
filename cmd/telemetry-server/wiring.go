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

	"github.com/tnando/my-robo-taxi-telemetry/internal/auth"
	"github.com/tnando/my-robo-taxi-telemetry/internal/config"
	"github.com/tnando/my-robo-taxi-telemetry/internal/cryptox"
	"github.com/tnando/my-robo-taxi-telemetry/internal/events"
	"github.com/tnando/my-robo-taxi-telemetry/internal/geocode"
	"github.com/tnando/my-robo-taxi-telemetry/internal/server"
	"github.com/tnando/my-robo-taxi-telemetry/internal/store"
	"github.com/tnando/my-robo-taxi-telemetry/internal/store/accountbackfill"
	"github.com/tnando/my-robo-taxi-telemetry/internal/store/routeblobbackfill"
	"github.com/tnando/my-robo-taxi-telemetry/internal/store/vehiclegpsbackfill"
	"github.com/tnando/my-robo-taxi-telemetry/internal/telemetry"
	"github.com/tnando/my-robo-taxi-telemetry/internal/ws"
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

// setupEncryption loads the AES-256-GCM key set so the binary fails-fast
// on missing or invalid ENCRYPTION_KEY at startup (NFR-3.23, NFR-3.24).
// The Encryptor is returned for the store layer; the per-version
// `cryptox_decrypt_total` counter is registered against reg and pre-
// populated with one zero-valued series per readable key version so
// /metrics shows the full label set on the first scrape. See
// docs/contracts/key-rotation.md and docs/contracts/data-classification.md
// §3.3 for the encryption contract; key-rotation.md §"Procedure" step 6
// is the operator-facing consumer of the counter.
func setupEncryption(reg prometheus.Registerer, logger *slog.Logger) (cryptox.Encryptor, error) {
	keySet, err := cryptox.LoadKeySetFromEnv()
	if err != nil {
		return nil, fmt.Errorf("loading encryption key set: %w", err)
	}
	metrics := cryptox.NewPrometheusMetrics(reg, keySet.ReadableVersions())
	encryptor, err := cryptox.NewEncryptor(keySet, cryptox.WithMetrics(metrics))
	if err != nil {
		return nil, fmt.Errorf("constructing encryptor: %w", err)
	}
	logger.Info("encryptor initialized",
		slog.Int("write_version", int(keySet.WriteVersion())),
		slog.Int("readable_versions", len(keySet.ReadableVersions())),
	)
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

// setupAuthenticator returns a NoopAuthenticator in dev mode (accepts any
// token) or a JWTAuthenticator wired against the auth secret + DB pool in
// production mode.
func setupAuthenticator(cfg *config.Config, dbPool *pgxpool.Pool, devMode bool, logger *slog.Logger) ws.Authenticator {
	if devMode {
		logger.Warn("dev mode enabled: WebSocket auth disabled, accepting any token")
		return &ws.NoopAuthenticator{}
	}
	logger.Info("JWT authentication enabled for WebSocket clients")
	return auth.NewJWTAuthenticator(
		cfg.Auth().Secret,
		cfg.Auth().TokenIssuer,
		cfg.Auth().TokenAudience,
		dbPool,
	)
}

// httpRouteDeps bundles the dependencies required to register the HTTP
// route surface. Grouped into a struct so setupHTTPHandlers's signature
// stays readable and so adding a new dep doesn't ripple through call
// sites.
type httpRouteDeps struct {
	cfg            *config.Config
	srv            *server.Server
	hub            *ws.Hub
	authenticator  ws.Authenticator
	recv           *telemetry.Receiver
	bus            events.Bus
	vinCache       *store.VINCache
	accountRepo    *store.AccountRepo
	debugGate      debugFieldsGate
	originPatterns []string
	logger         *slog.Logger
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

	setupFleetConfigEndpoint(deps.cfg, deps.srv, deps.authenticator, deps.vinCache, deps.accountRepo, deps.logger)

	// Mounted when resolveDebugFieldsGate says so — either because the
	// server is running with --dev (token optional) or because an operator
	// has set DEBUG_FIELDS_TOKEN on a production instance to let
	// `ops fields watch` stream real-Tesla frames. Auth is enforced by
	// DebugFieldsHandler via the X-Debug-Token header / ?token= query
	// param when APIKey is non-empty.
	if deps.debugGate.Enabled {
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
