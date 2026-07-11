package config

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// fileConfig mirrors the JSON structure for unmarshaling. All duration
// fields use the Duration type which parses from strings like "5s".
type fileConfig struct {
	Server    fileServerConfig    `json:"server"`
	TLS       fileTLSConfig       `json:"tls"`
	Database  fileDatabaseConfig  `json:"database"`
	Telemetry fileTelemetryConfig `json:"telemetry"`
	Drives    fileDrivesConfig    `json:"drives"`
	WebSocket fileWebSocketConfig `json:"websocket"`
	Auth      fileAuthConfig      `json:"auth"`
	Identity  fileIdentityConfig  `json:"identity"`
	Proxy     fileProxyConfig     `json:"proxy"`

	// Populated from environment, not JSON.
	databaseURL          string
	authSecret           string
	mapboxToken          string
	teslaPublicKey       string
	fleetTelemetryCA     string
	teslaClientID        string
	teslaClientSec       string
	certMonitorEndpoints []string
	dispatchEnabled      bool

	// Identity module secrets/identifiers (env, not JSON).
	es256PrivateKeyPEM string
	appleClientID      string
	appleBootstrap     map[string]string
}

type fileServerConfig struct {
	TeslaPort   int `json:"tesla_port"`
	ClientPort  int `json:"client_port"`
	MetricsPort int `json:"metrics_port"`
}

type fileTLSConfig struct {
	CertFile string `json:"cert_file"`
	KeyFile  string `json:"key_file"`
	CAFile   string `json:"ca_file"`
}

type fileDatabaseConfig struct {
	MaxConns                  int  `json:"max_conns"`
	MinConns                  int  `json:"min_conns"`
	DisablePreparedStatements bool `json:"disable_prepared_statements"`
}

type fileTelemetryConfig struct {
	MaxVehicles        int      `json:"max_vehicles"`
	EventBufferSize    int      `json:"event_buffer_size"`
	BatchWriteInterval Duration `json:"batch_write_interval"`
	BatchWriteSize     int      `json:"batch_write_size"`
}

type fileDrivesConfig struct {
	MinDuration      Duration `json:"min_duration"`
	MinDistanceMiles float64  `json:"min_distance_miles"`
	EndDebounce      Duration `json:"end_debounce"`
	GeocodeTimeout   Duration `json:"geocode_timeout"`
	StallTimeout     Duration `json:"stall_timeout"`
	MaxDriveDuration Duration `json:"max_drive_duration"`
}

type fileWebSocketConfig struct {
	HeartbeatInterval     Duration `json:"heartbeat_interval"`
	WriteTimeout          Duration `json:"write_timeout"`
	MaxConnectionsPerUser int      `json:"max_connections_per_user"`
	ReadLimit             int64    `json:"read_limit"`
	AllowedOrigins        []string `json:"allowed_origins"`
}

type fileAuthConfig struct {
	TokenIssuer   string `json:"token_issuer"`
	TokenAudience string `json:"token_audience"`
}

type fileIdentityConfig struct {
	AccessTokenTTL         Duration `json:"access_token_ttl"`
	RefreshTokenTTL        Duration `json:"refresh_token_ttl"`
	AuthRateLimitPerMinute int      `json:"auth_rate_limit_per_minute"`
	AuthRateLimitBurst     int      `json:"auth_rate_limit_burst"`
}

type fileProxyConfig struct {
	URL                    string `json:"url"`
	FleetTelemetryHostname string `json:"fleet_telemetry_hostname"`
	FleetTelemetryPort     int    `json:"fleet_telemetry_port"`
}

// loadFile reads and decodes the JSON configuration from disk.
func loadFile(path string) (*fileConfig, error) {
	f, err := os.Open(path) // #nosec G304 -- path is caller-controlled startup config, not user input
	if err != nil {
		return nil, fmt.Errorf("config.Load: open %q: %w: %w", path, ErrConfigLoad, err)
	}
	defer f.Close()

	var fc fileConfig
	if err := json.NewDecoder(f).Decode(&fc); err != nil {
		return nil, fmt.Errorf("config.Load: decode %q: %w: %w", path, ErrConfigLoad, err)
	}
	return &fc, nil
}

// applyDefaults fills zero-value fields with sensible production defaults.
func applyDefaults(fc *fileConfig) {
	applyServerDefaults(&fc.Server)
	applyDatabaseDefaults(&fc.Database)
	applyTelemetryDefaults(&fc.Telemetry)
	applyDrivesDefaults(&fc.Drives)
	applyWebSocketDefaults(&fc.WebSocket)
	applyAuthDefaults(&fc.Auth)
	applyIdentityDefaults(&fc.Identity)
	applyProxyDefaults(&fc.Proxy)
}

// applyEnvOverrides reads secrets and optional overrides from environment
// variables. Returns an error naming any missing required variable.
func applyEnvOverrides(fc *fileConfig) error {
	var missing []string

	fc.databaseURL = os.Getenv("DATABASE_URL")
	if fc.databaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}

	fc.authSecret = os.Getenv("AUTH_SECRET")
	if fc.authSecret == "" {
		missing = append(missing, "AUTH_SECRET")
	}

	fc.mapboxToken = os.Getenv("MAPBOX_TOKEN")            // optional
	fc.teslaPublicKey = os.Getenv("TESLA_PUBLIC_KEY")     // optional
	fc.fleetTelemetryCA = os.Getenv("FLEET_TELEMETRY_CA") // optional: PEM CA cert
	fc.teslaClientID = os.Getenv("AUTH_TESLA_ID")         // optional: enables token refresh
	fc.teslaClientSec = os.Getenv("AUTH_TESLA_SECRET")    // optional: enables token refresh

	// Identity module (MYR-193, ADR-001). All optional: absent = module
	// disabled (the server stays a pure token VALIDATOR, HS256-only).
	keyPEM, err := readES256KeyEnv()
	if err != nil {
		return err
	}
	fc.es256PrivateKeyPEM = keyPEM
	fc.appleClientID = os.Getenv("APPLE_NATIVE_CLIENT_ID")   // expected aud on Apple id tokens
	fc.appleBootstrap = parseKVMap(os.Getenv("AUTH_APPLE_BOOTSTRAP")) // email=cuid[,email=cuid]

	// Database env var overrides.
	if v := os.Getenv("DATABASE_DISABLE_PREPARED_STATEMENTS"); v == "true" || v == "1" {
		fc.Database.DisablePreparedStatements = true
	}

	// Proxy env var overrides.
	if v := os.Getenv("TESLA_PROXY_URL"); v != "" {
		fc.Proxy.URL = v
	}

	// DISPATCH_ENABLED is the MYR-176 nav-dispatch kill-switch. Defaults to
	// true (dispatch on) when unset. When set it MUST be a strconv.ParseBool
	// value (1/t/T/TRUE/true/True/0/f/F/FALSE/false/False); anything else is a
	// config error and fails fast at startup (repo rule: validate config at
	// startup — do not silently fall back to "on" on a typo like "no"/"off").
	fc.dispatchEnabled = true
	if v, ok := os.LookupEnv("DISPATCH_ENABLED"); ok {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("config.Load: %w: DISPATCH_ENABLED=%q is not a boolean", ErrInvalidValue, v)
		}
		fc.dispatchEnabled = enabled
	}

	// TLS env vars override JSON values.
	if v := os.Getenv("TLS_CERT_FILE"); v != "" {
		fc.TLS.CertFile = v
	}
	if v := os.Getenv("TLS_KEY_FILE"); v != "" {
		fc.TLS.KeyFile = v
	}
	if v := os.Getenv("TLS_CA_FILE"); v != "" {
		fc.TLS.CAFile = v
	}

	// WEBSOCKET_ALLOWED_ORIGINS overrides websocket.allowed_origins.
	// Comma-separated list of origin patterns matched against the Origin
	// header by coder/websocket — patterns containing "://" are matched
	// against scheme+host, otherwise against host only. Each entry is
	// trimmed of surrounding whitespace; empty entries are dropped. Set
	// to a single space (or any string with no non-empty entries) to
	// signal "explicitly empty" (fail-closed). Per NFR-3.22 and MYR-17.
	if v, ok := os.LookupEnv("WEBSOCKET_ALLOWED_ORIGINS"); ok {
		fc.WebSocket.AllowedOrigins = parseOriginList(v)
	}

	// TLS_MONITOR_ENDPOINTS is a comma-separated list of host:port
	// addresses the endpoint cert monitor probes (e.g.
	// "telemetry.myrobotaxi.app:443,telemetry.myrobotaxi.app:4443").
	// Unset/empty disables the monitor.
	if v, ok := os.LookupEnv("TLS_MONITOR_ENDPOINTS"); ok {
		fc.certMonitorEndpoints = parseOriginList(v)
	}

	if len(missing) > 0 {
		return fmt.Errorf("config.Load: %w: %v", ErrMissingRequired, missing)
	}
	return nil
}

// readES256KeyEnv reads the identity module's ES256 private key from the
// environment. AUTH_ES256_PRIVATE_KEY holds the raw PKCS#8 PEM;
// AUTH_ES256_PRIVATE_KEY_B64 holds the same PEM base64-encoded (the Fly /
// container convention — a single env line with no embedded newlines). The
// raw form wins if both are set. Returns "" (no key) when neither is set.
func readES256KeyEnv() (string, error) {
	if raw := os.Getenv("AUTH_ES256_PRIVATE_KEY"); raw != "" {
		return raw, nil
	}
	b64 := os.Getenv("AUTH_ES256_PRIVATE_KEY_B64")
	if b64 == "" {
		return "", nil
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return "", fmt.Errorf("config.Load: decode AUTH_ES256_PRIVATE_KEY_B64: %w: %w", ErrInvalidValue, err)
	}
	return string(decoded), nil
}

// parseKVMap parses a "k=v,k=v" list into a map. Whitespace around keys and
// values is trimmed; empty or malformed (no "=") entries are dropped. Keys
// are lower-cased so email lookups are case-insensitive. Returns nil for an
// empty input so an unset var yields a nil (disabled) map.
func parseKVMap(raw string) map[string]string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	out := make(map[string]string)
	for _, pair := range strings.Split(raw, ",") {
		k, v, ok := strings.Cut(pair, "=")
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseOriginList splits a comma-separated origin pattern list, trims
// whitespace, and drops empty entries. A bare "*" is preserved (callers
// decide whether to allow it).
func parseOriginList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// buildConfig converts the intermediate fileConfig into an immutable Config.
func buildConfig(fc *fileConfig) *Config {
	return &Config{
		server: ServerConfig{
			TeslaPort:   fc.Server.TeslaPort,
			ClientPort:  fc.Server.ClientPort,
			MetricsPort: fc.Server.MetricsPort,
		},
		tls: TLSConfig{
			CertFile: fc.TLS.CertFile,
			KeyFile:  fc.TLS.KeyFile,
			CAFile:   fc.TLS.CAFile,
		},
		database: DatabaseConfig{
			URL:                       fc.databaseURL,
			MaxConns:                  fc.Database.MaxConns,
			MinConns:                  fc.Database.MinConns,
			DisablePreparedStatements: fc.Database.DisablePreparedStatements,
		},
		telemetry: TelemetryConfig{
			MaxVehicles:        fc.Telemetry.MaxVehicles,
			EventBufferSize:    fc.Telemetry.EventBufferSize,
			BatchWriteInterval: fc.Telemetry.BatchWriteInterval.Dur(),
			BatchWriteSize:     fc.Telemetry.BatchWriteSize,
		},
		drives: DrivesConfig{
			MinDuration:      fc.Drives.MinDuration.Dur(),
			MinDistanceMiles: fc.Drives.MinDistanceMiles,
			EndDebounce:      fc.Drives.EndDebounce.Dur(),
			GeocodeTimeout:   fc.Drives.GeocodeTimeout.Dur(),
			StallTimeout:     fc.Drives.StallTimeout.Dur(),
			MaxDriveDuration: fc.Drives.MaxDriveDuration.Dur(),
		},
		websocket: WebSocketConfig{
			HeartbeatInterval:     fc.WebSocket.HeartbeatInterval.Dur(),
			WriteTimeout:          fc.WebSocket.WriteTimeout.Dur(),
			MaxConnectionsPerUser: fc.WebSocket.MaxConnectionsPerUser,
			ReadLimit:             fc.WebSocket.ReadLimit,
			AllowedOrigins:        fc.WebSocket.AllowedOrigins,
		},
		auth: AuthConfig{
			Secret:        fc.authSecret,
			TokenIssuer:   fc.Auth.TokenIssuer,
			TokenAudience: fc.Auth.TokenAudience,
		},
		identity: IdentityConfig{
			ES256PrivateKeyPEM:     fc.es256PrivateKeyPEM,
			AppleClientID:          fc.appleClientID,
			BootstrapEmailToCUID:   fc.appleBootstrap,
			AccessTokenTTL:         fc.Identity.AccessTokenTTL.Dur(),
			RefreshTokenTTL:        fc.Identity.RefreshTokenTTL.Dur(),
			AuthRateLimitPerMinute: fc.Identity.AuthRateLimitPerMinute,
			AuthRateLimitBurst:     fc.Identity.AuthRateLimitBurst,
		},
		proxy: ProxyConfig{
			URL:                    fc.Proxy.URL,
			FleetTelemetryHostname: fc.Proxy.FleetTelemetryHostname,
			FleetTelemetryPort:     fc.Proxy.FleetTelemetryPort,
			FleetTelemetryCA:       fc.fleetTelemetryCA,
		},
		teslaOAuth: TeslaOAuthConfig{
			ClientID:     fc.teslaClientID,
			ClientSecret: fc.teslaClientSec,
		},
		monitoring: MonitoringConfig{
			CertEndpoints: fc.certMonitorEndpoints,
		},
		mapboxToken:     fc.mapboxToken,
		teslaPublicKey:  fc.teslaPublicKey,
		dispatchEnabled: fc.dispatchEnabled,
	}
}
