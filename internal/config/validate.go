package config

import (
	"fmt"
	"strings"
)

// validate checks a fully-assembled Config for invalid values. It collects
// all errors rather than stopping at the first, so the operator sees every
// problem in one pass.
func validate(cfg *Config) error {
	errs := make([]string, 0, 8) //nolint:mnd // reasonable initial capacity

	errs = append(errs, validateServer(cfg.server)...)
	errs = append(errs, validateDatabase(cfg.database)...)
	errs = append(errs, validateTelemetry(cfg.telemetry)...)
	errs = append(errs, validateDrives(cfg.drives)...)
	errs = append(errs, validateWebSocket(cfg.websocket)...)
	errs = append(errs, validateAuth(cfg.auth)...)

	if len(errs) > 0 {
		return fmt.Errorf("config.Validate: %w:\n  %s",
			ErrInvalidValue, strings.Join(errs, "\n  "))
	}
	return nil
}

func validateServer(s ServerConfig) []string {
	var errs []string
	if !validPort(s.TeslaPort) {
		errs = append(errs, fmt.Sprintf("server.tesla_port: %d is not a valid port (1-65535)", s.TeslaPort))
	}
	if !validPort(s.ClientPort) {
		errs = append(errs, fmt.Sprintf("server.client_port: %d is not a valid port (1-65535)", s.ClientPort))
	}
	if !validPort(s.MetricsPort) {
		errs = append(errs, fmt.Sprintf("server.metrics_port: %d is not a valid port (1-65535)", s.MetricsPort))
	}
	if s.TeslaPort == s.ClientPort {
		errs = append(errs, fmt.Sprintf("server.tesla_port and server.client_port must differ (both %d)", s.TeslaPort))
	}
	if s.TeslaPort == s.MetricsPort {
		errs = append(errs, fmt.Sprintf("server.tesla_port and server.metrics_port must differ (both %d)", s.TeslaPort))
	}
	if s.ClientPort == s.MetricsPort {
		errs = append(errs, fmt.Sprintf("server.client_port and server.metrics_port must differ (both %d)", s.ClientPort))
	}
	return errs
}

func validateDatabase(d DatabaseConfig) []string {
	var errs []string
	if d.URL == "" {
		errs = append(errs, "database.url: must not be empty")
	}
	if d.MaxConns < 1 {
		errs = append(errs, fmt.Sprintf("database.max_conns: %d must be >= 1", d.MaxConns))
	}
	if d.MinConns < 0 {
		errs = append(errs, fmt.Sprintf("database.min_conns: %d must be >= 0", d.MinConns))
	}
	if d.MinConns > d.MaxConns {
		errs = append(errs, fmt.Sprintf("database.min_conns (%d) must be <= database.max_conns (%d)", d.MinConns, d.MaxConns))
	}
	return errs
}

func validateTelemetry(t TelemetryConfig) []string {
	var errs []string
	if t.MaxVehicles < 1 {
		errs = append(errs, fmt.Sprintf("telemetry.max_vehicles: %d must be >= 1", t.MaxVehicles))
	}
	if t.EventBufferSize < 1 {
		errs = append(errs, fmt.Sprintf("telemetry.event_buffer_size: %d must be >= 1", t.EventBufferSize))
	}
	if t.BatchWriteInterval <= 0 {
		errs = append(errs, "telemetry.batch_write_interval: must be positive")
	}
	if t.BatchWriteSize < 1 {
		errs = append(errs, fmt.Sprintf("telemetry.batch_write_size: %d must be >= 1", t.BatchWriteSize))
	}
	return errs
}

func validateDrives(d DrivesConfig) []string {
	var errs []string
	if d.MinDuration <= 0 {
		errs = append(errs, "drives.min_duration: must be positive")
	}
	if d.MinDistanceMiles <= 0 {
		errs = append(errs, fmt.Sprintf("drives.min_distance_miles: %g must be positive", d.MinDistanceMiles))
	}
	if d.EndDebounce <= 0 {
		errs = append(errs, "drives.end_debounce: must be positive")
	}
	if d.GeocodeTimeout <= 0 {
		errs = append(errs, "drives.geocode_timeout: must be positive")
	}
	return errs
}

func validateWebSocket(ws WebSocketConfig) []string {
	var errs []string
	if ws.HeartbeatInterval <= 0 {
		errs = append(errs, "websocket.heartbeat_interval: must be positive")
	}
	if ws.WriteTimeout <= 0 {
		errs = append(errs, "websocket.write_timeout: must be positive")
	}
	if ws.MaxConnectionsPerUser < 1 {
		errs = append(errs, fmt.Sprintf("websocket.max_connections_per_user: %d must be >= 1", ws.MaxConnectionsPerUser))
	}
	if ws.ReadLimit < 1 {
		errs = append(errs, fmt.Sprintf("websocket.read_limit: %d must be >= 1", ws.ReadLimit))
	}
	return errs
}

func validateAuth(a AuthConfig) []string {
	var errs []string
	if a.Secret == "" {
		errs = append(errs, "auth.secret: must not be empty")
	}
	if a.TokenIssuer == "" {
		errs = append(errs, "auth.token_issuer: must not be empty")
	}
	if a.TokenAudience == "" {
		errs = append(errs, "auth.token_audience: must not be empty")
	}
	return errs
}

// validPort returns true if port is in the valid TCP range 1-65535.
func validPort(port int) bool {
	return port >= 1 && port <= 65535
}
