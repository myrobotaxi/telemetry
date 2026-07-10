// Package config loads, validates, and provides access to application
// configuration. Settings come from two sources: a JSON file for
// operational parameters and environment variables for secrets.
// After loading, the Config is immutable — all access is through getters.
package config

import "time"

// Config holds the fully-validated, immutable application configuration.
// All access is through getter methods — there are no exported setters.
type Config struct {
	server         ServerConfig
	tls            TLSConfig
	database       DatabaseConfig
	telemetry      TelemetryConfig
	drives         DrivesConfig
	websocket      WebSocketConfig
	auth           AuthConfig
	identity       IdentityConfig
	proxy          ProxyConfig
	teslaOAuth     TeslaOAuthConfig
	monitoring     MonitoringConfig
	mapboxToken    string
	teslaPublicKey string
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

// ProxyConfig holds settings for the Tesla Fleet API proxy (tesla-http-proxy).
// All fields are optional — when URL is empty, the fleet config push endpoint
// is disabled.
type ProxyConfig struct {
	URL                    string
	FleetTelemetryHostname string
	FleetTelemetryPort     int
	FleetTelemetryCA       string // PEM-encoded CA cert
}

// TeslaOAuthConfig holds the Tesla OAuth2 application credentials needed to
// refresh expired tokens. Both fields are optional — when ClientID is empty,
// automatic token refresh is disabled.
type TeslaOAuthConfig struct {
	ClientID     string
	ClientSecret string
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

// Monitoring returns the observability probe configuration. When
// CertEndpoints is empty, the endpoint TLS cert monitor is disabled.
func (c *Config) Monitoring() MonitoringConfig { return c.monitoring }

// MapboxToken returns the Mapbox API token. Empty string means geocoding
// is disabled.
func (c *Config) MapboxToken() string { return c.mapboxToken }

// TeslaPublicKey returns the PEM-encoded public key for the Tesla
// .well-known endpoint. Empty string disables the endpoint.
func (c *Config) TeslaPublicKey() string { return c.teslaPublicKey }

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
