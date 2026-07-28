// Package config loads, validates, and provides access to application
// configuration. Settings come from two sources: a JSON file for
// operational parameters and environment variables for secrets.
// After loading, the Config is immutable — all access is through getters.
package config

import "time"

// Config holds the fully-validated, immutable application configuration.
// All access is through getter methods — there are no exported setters.
type Config struct {
	server          ServerConfig
	tls             TLSConfig
	database        DatabaseConfig
	telemetry       TelemetryConfig
	drives          DrivesConfig
	websocket       WebSocketConfig
	auth            AuthConfig
	identity        IdentityConfig
	proxy           ProxyConfig
	teslaOAuth      TeslaOAuthConfig
	teslaLink       TeslaLinkConfig
	monitoring      MonitoringConfig
	mapboxToken     string
	teslaPublicKey  string
	dispatchEnabled bool
	// reservationDispatchEnabled is the MYR-179 scheduled-dispatch sweeper
	// kill-switch, independent of dispatchEnabled.
	reservationDispatchEnabled bool
}

// MonitoringConfig holds observability probe settings.
type MonitoringConfig struct {
	// CertEndpoints is the list of host:port addresses the endpoint TLS
	// certificate monitor probes on a schedule (see EndpointCertMonitor).
	// It covers certs terminated outside the app process — notably the
	// Fly-managed cert on the client WebSocket port, which the file-based
	// monitor cannot see. Empty disables the monitor. Populated from the
	// TLS_MONITOR_ENDPOINTS env var (comma-separated).
	CertEndpoints []string
}

// ServerConfig holds port bindings for the three HTTP listeners.
type ServerConfig struct {
	TeslaPort   int
	ClientPort  int
	MetricsPort int
}

// TLSConfig holds paths to TLS certificates and the Tesla CA.
type TLSConfig struct {
	CertFile string
	KeyFile  string
	CAFile   string
}

// DatabaseConfig holds connection pool parameters and the connection URL.
type DatabaseConfig struct {
	URL                       string
	MaxConns                  int
	MinConns                  int
	DisablePreparedStatements bool // Set true for PgBouncer transaction pooling (Supabase port 6543)
}

// TelemetryConfig holds tuning parameters for the telemetry receiver.
type TelemetryConfig struct {
	MaxVehicles        int
	EventBufferSize    int
	BatchWriteInterval time.Duration
	BatchWriteSize     int
}

// DrivesConfig holds parameters for drive detection and geocoding.
type DrivesConfig struct {
	MinDuration      time.Duration
	MinDistanceMiles float64
	EndDebounce      time.Duration
	GeocodeTimeout   time.Duration

	// StallTimeout ends an active drive when telemetry keeps flowing
	// but no movement (speed, new route point, odometer advance) has
	// been observed for this long. Guards against a missed gear=P
	// frame leaving a drive open while the parked car streams idle
	// telemetry (MYR-160). Must exceed plausible in-drive stops
	// (drive-thrus, rail crossings).
	StallTimeout time.Duration

	// MaxDriveDuration is a hard cap on the age of an active drive.
	// A backstop for the pathological case where movement keeps being
	// (mis)observed indefinitely; any legitimate drive this long is
	// split into two rows rather than risking a stuck-open row
	// (MYR-160).
	MaxDriveDuration time.Duration
}

// WebSocketConfig holds parameters for the client-facing WebSocket server.
type WebSocketConfig struct {
	HeartbeatInterval     time.Duration
	WriteTimeout          time.Duration
	MaxConnectionsPerUser int
	ReadLimit             int64
	AllowedOrigins        []string
}

// AuthConfig holds JWT validation parameters shared with NextAuth.js.
type AuthConfig struct {
	Secret        string
	TokenIssuer   string
	TokenAudience string
}

// IdentityConfig holds settings for the internal/identity module (MYR-193,
// ADR-001): native Sign in with Apple, ES256 access-token minting, and
// rotating refresh tokens. Secrets (the ES256 private key) come from the
// environment; the operational knobs (TTLs, rate limit) come from the JSON
// file. The module is ENABLED at wiring time when an ES256 signing key is
// available (a config key in prod, or an ephemeral dev key) AND an Apple
// client id is configured.
type IdentityConfig struct {
	// ES256PrivateKeyPEM is the PKCS#8 PEM of the P-256 private key used to
	// sign access tokens. Secret — from AUTH_ES256_PRIVATE_KEY(_B64). Empty
	// means "no static key"; in --dev the wiring generates an ephemeral one.
	ES256PrivateKeyPEM string

	// AppleClientID is the expected `aud` on Apple identity tokens — the
	// native app's Service/Bundle id (APPLE_NATIVE_CLIENT_ID,
	// e.g. app.myrobotaxi.ios). Empty disables the Apple sign-in endpoint.
	AppleClientID string

	// BootstrapEmailToCUID is a config-seeded first-sign-in override
	// (AUTH_APPLE_BOOTSTRAP) mapping a verified email to an existing user
	// CUID, so a known user's first Apple sign-in binds to the right CUID
	// even if email-match would miss (e.g. Apple private-relay address).
	BootstrapEmailToCUID map[string]string

	// AccessTokenTTL is the lifetime of a minted ES256 access token (~1h).
	AccessTokenTTL time.Duration

	// RefreshTokenTTL is the sliding refresh-family lifetime (~90d): each
	// rotation issues a refresh token expiring now + RefreshTokenTTL.
	RefreshTokenTTL time.Duration

	// AuthRateLimitPerMinute is the per-IP request cap on the /api/auth/*
	// endpoints (brute-force protection). Burst is AuthRateLimitBurst.
	AuthRateLimitPerMinute int
	AuthRateLimitBurst     int
}

// ProxyConfig holds settings for the Tesla Fleet API proxy (tesla-http-proxy)
// and the direct Fleet REST base. URL/FleetTelemetry* are optional — when URL
// is empty, the fleet config push endpoint is disabled. FleetAPIBaseURL is
// REQUIRED and validated at startup: unsigned commands (navigation_request)
// are routed straight to it, NOT through the proxy, because tesla-http-proxy
// v0.4.1 mis-forwards REST-API commands (MYR-245).
type ProxyConfig struct {
	URL                    string
	FleetTelemetryHostname string
	FleetTelemetryPort     int
	FleetTelemetryCA       string // PEM-encoded CA cert

	// FleetAPIBaseURL is the Tesla Fleet API origin (scheme + host, no path)
	// for direct REST command calls. Defaults to the North-America prod region
	// (https://fleet-api.prd.na.vn.cloud.tesla.com). Must parse as an https URL
	// — validated fail-fast at startup so unsigned commands never silently fall
	// back to the (mis-forwarding) proxy.
	FleetAPIBaseURL string
}

// TeslaOAuthConfig holds the Tesla OAuth2 application credentials needed to
// refresh expired tokens. Both fields are optional — when ClientID is empty,
// automatic token refresh is disabled.
type TeslaOAuthConfig struct {
	ClientID     string
	ClientSecret string
}

// TeslaLinkConfig holds settings for the user-facing in-app Tesla OAuth link
// endpoints (MYR-246, internal/teslalink). The link surface is ENABLED only
// when RedirectBaseURL is non-empty AND Tesla OAuth credentials (AUTH_TESLA_ID/
// SECRET, see TeslaOAuthConfig) are configured. Both fields come from the
// environment.
type TeslaLinkConfig struct {
	// RedirectBaseURL is the public origin (scheme + host[:port], no trailing
	// slash) at which this server's client mux is reachable by Tesla's browser
	// redirect — e.g. https://telemetry.myrobotaxi.app:4443. The callback route
	// is RedirectBaseURL + "/api/tesla/link/callback"; THIS exact URI must be
	// registered as an Allowed Redirect URI on the Tesla Fleet app. Empty
	// disables the in-app link endpoints. From TESLA_LINK_REDIRECT_BASE_URL.
	RedirectBaseURL string

	// AppRedirectURL is the app deep link the callback hands off to after the
	// token exchange (ASWebAuthenticationSession intercepts it). Defaults to
	// "myrobotaxi://tesla-linked". From TESLA_LINK_APP_REDIRECT.
	AppRedirectURL string
}

// Getters — one per section, returning a copy of the section struct.

// Server returns the server port configuration.
func (c *Config) Server() ServerConfig { return c.server }

// TLS returns the TLS certificate paths.
func (c *Config) TLS() TLSConfig { return c.tls }

// Database returns the database connection configuration.
func (c *Config) Database() DatabaseConfig { return c.database }

// Telemetry returns the telemetry receiver configuration.
func (c *Config) Telemetry() TelemetryConfig { return c.telemetry }

// Drives returns the drive detection configuration.
func (c *Config) Drives() DrivesConfig { return c.drives }

// WebSocket returns the WebSocket server configuration.
func (c *Config) WebSocket() WebSocketConfig { return c.websocket }

// Auth returns the authentication configuration.
func (c *Config) Auth() AuthConfig { return c.auth }

// Identity returns the identity-module configuration (MYR-193, ADR-001).
func (c *Config) Identity() IdentityConfig { return c.identity }

// Proxy returns the Tesla Fleet API proxy configuration. When URL is empty,
// the fleet config push feature is unavailable.
func (c *Config) Proxy() ProxyConfig { return c.proxy }

// TeslaOAuth returns the Tesla OAuth2 application credentials. When
// ClientID is empty, automatic token refresh is disabled.
func (c *Config) TeslaOAuth() TeslaOAuthConfig { return c.teslaOAuth }

// TeslaLink returns the in-app Tesla OAuth link configuration (MYR-246). When
// RedirectBaseURL is empty, the /api/tesla/link/* endpoints are not mounted.
func (c *Config) TeslaLink() TeslaLinkConfig { return c.teslaLink }

// Monitoring returns the observability probe configuration. When
// CertEndpoints is empty, the endpoint TLS cert monitor is disabled.
func (c *Config) Monitoring() MonitoringConfig { return c.monitoring }

// MapboxToken returns the Mapbox API token. Empty string means geocoding
// is disabled.
func (c *Config) MapboxToken() string { return c.mapboxToken }

// TeslaPublicKey returns the PEM-encoded public key for the Tesla
// .well-known endpoint. Empty string disables the endpoint.
func (c *Config) TeslaPublicKey() string { return c.teslaPublicKey }

// DispatchEnabled is the MYR-176 nav-dispatch kill-switch. When false the
// dispatcher records every accepted ride as `skipped` and pushes no nav to
// the vehicle. Defaults to true; set DISPATCH_ENABLED=false to disable
// without a deploy.
func (c *Config) DispatchEnabled() bool { return c.dispatchEnabled }

// ReservationDispatchEnabled is the MYR-179 scheduled-dispatch kill-switch.
// When false the reservation sweeper does not run at all: a scheduled ride's
// pickup nav is never fired at its `scheduledFor` instant, and due
// reservations stay accepted / latch-unclaimed / outcome-absent (so enabling
// it later picks them up rather than having burned them). Deliberately
// separate from DispatchEnabled so scheduled dispatch can be stopped without
// affecting instant rides. Defaults to true; set
// RESERVATION_DISPATCH_ENABLED=false to disable without a deploy.
func (c *Config) ReservationDispatchEnabled() bool { return c.reservationDispatchEnabled }

// Load reads configuration from the JSON file at configPath, overlays
// environment variable overrides, applies defaults for missing optional
// fields, and validates the result. It returns an immutable Config or
// a descriptive error.
func Load(configPath string) (*Config, error) {
	fc, err := loadFile(configPath)
	if err != nil {
		return nil, err
	}

	applyDefaults(fc)

	if err := applyEnvOverrides(fc); err != nil {
		return nil, err
	}

	cfg := buildConfig(fc)

	if err := validate(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}
