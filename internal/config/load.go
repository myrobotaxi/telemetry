package config

import (
	"encoding/json"
	"fmt"
	"os"
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
	Proxy     fileProxyConfig     `json:"proxy"`

	// Populated from environment, not JSON.
	databaseURL      string
	authSecret       string
	mapboxToken      string
	teslaPublicKey   string
	fleetTelemetryCA string
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

	fc.mapboxToken = os.Getenv("MAPBOX_TOKEN")           // optional
	fc.teslaPublicKey = os.Getenv("TESLA_PUBLIC_KEY")   // optional
	fc.fleetTelemetryCA = os.Getenv("FLEET_TELEMETRY_CA") // optional: PEM CA cert

	// Database env var overrides.
	if v := os.Getenv("DATABASE_DISABLE_PREPARED_STATEMENTS"); v == "true" || v == "1" {
		fc.Database.DisablePreparedStatements = true
	}

	// Proxy env var overrides.
	if v := os.Getenv("TESLA_PROXY_URL"); v != "" {
		fc.Proxy.URL = v
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

	if len(missing) > 0 {
		return fmt.Errorf("config.Load: %w: %v", ErrMissingRequired, missing)
	}
	return nil
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
		proxy: ProxyConfig{
			URL:                    fc.Proxy.URL,
			FleetTelemetryHostname: fc.Proxy.FleetTelemetryHostname,
			FleetTelemetryPort:     fc.Proxy.FleetTelemetryPort,
			FleetTelemetryCA:       fc.fleetTelemetryCA,
		},
		mapboxToken:    fc.mapboxToken,
		teslaPublicKey: fc.teslaPublicKey,
	}
}
