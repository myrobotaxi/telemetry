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
	proxy          ProxyConfig
	mapboxToken    string
	teslaPublicKey string
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

// ProxyConfig holds settings for the Tesla Fleet API proxy (tesla-http-proxy).
// All fields are optional — when URL is empty, the fleet config push endpoint
// is disabled.
type ProxyConfig struct {
	URL                    string
	FleetTelemetryHostname string
	FleetTelemetryPort     int
	FleetTelemetryCA       string // PEM-encoded CA cert
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

// Proxy returns the Tesla Fleet API proxy configuration. When URL is empty,
// the fleet config push feature is unavailable.
func (c *Config) Proxy() ProxyConfig { return c.proxy }

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
