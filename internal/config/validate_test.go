package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoad_ValidationErrors(t *testing.T) {
	tests := []struct {
		name       string
		overrides  map[string]any
		wantSubstr string
	}{
		{
			name: "negative tesla port",
			overrides: map[string]any{
				"server": map[string]any{
					"tesla_port":   -1,
					"client_port":  8080,
					"metrics_port": 9090,
				},
			},
			wantSubstr: "server.tesla_port",
		},
		{
			name: "port too high",
			overrides: map[string]any{
				"server": map[string]any{
					"tesla_port":   70000,
					"client_port":  8080,
					"metrics_port": 9090,
				},
			},
			wantSubstr: "server.tesla_port",
		},
		{
			name: "duplicate ports tesla and client",
			overrides: map[string]any{
				"server": map[string]any{
					"tesla_port":   8080,
					"client_port":  8080,
					"metrics_port": 9090,
				},
			},
			wantSubstr: "must differ",
		},
		{
			name: "duplicate ports client and metrics",
			overrides: map[string]any{
				"server": map[string]any{
					"tesla_port":   443,
					"client_port":  9090,
					"metrics_port": 9090,
				},
			},
			wantSubstr: "must differ",
		},
		{
			name: "min_conns exceeds max_conns",
			overrides: map[string]any{
				"database": map[string]any{
					"max_conns": 5,
					"min_conns": 10,
				},
			},
			wantSubstr: "database.min_conns",
		},
		{
			name: "negative batch_write_size",
			overrides: map[string]any{
				"telemetry": map[string]any{
					"max_vehicles":         100,
					"event_buffer_size":    1000,
					"batch_write_interval": "5s",
					"batch_write_size":     -1,
				},
			},
			wantSubstr: "telemetry.batch_write_size",
		},
		{
			name: "negative min_distance_miles",
			overrides: map[string]any{
				"drives": map[string]any{
					"min_duration":       "2m",
					"min_distance_miles": -0.5,
					"end_debounce":       "30s",
					"geocode_timeout":    "5s",
				},
			},
			wantSubstr: "drives.min_distance_miles",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTestConfig(t, dir, tt.overrides)
			setRequiredEnv(t)

			_, err := Load(path)
			if err == nil {
				t.Fatal("Load() expected validation error, got nil")
			}
			if !errors.Is(err, ErrInvalidValue) {
				t.Errorf("error should wrap ErrInvalidValue, got: %v", err)
			}
			if !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Errorf("error %q should contain %q", err.Error(), tt.wantSubstr)
			}
		})
	}
}

func TestLoad_MultipleValidationErrors(t *testing.T) {
	dir := t.TempDir()
	path := writeTestConfig(t, dir, map[string]any{
		"server": map[string]any{
			"tesla_port":   -1,
			"client_port":  -1,
			"metrics_port": -1,
		},
	})
	setRequiredEnv(t)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() expected validation error, got nil")
	}

	msg := err.Error()
	if !strings.Contains(msg, "server.tesla_port") {
		t.Errorf("error should mention tesla_port: %s", msg)
	}
	if !strings.Contains(msg, "server.client_port") {
		t.Errorf("error should mention client_port: %s", msg)
	}
	if !strings.Contains(msg, "server.metrics_port") {
		t.Errorf("error should mention metrics_port: %s", msg)
	}
}

func TestLoad_InvalidDurationString(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	data := []byte(`{
		"telemetry": {
			"batch_write_interval": "not-a-duration"
		}
	}`)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	setRequiredEnv(t)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() expected error for invalid duration, got nil")
	}
	if !errors.Is(err, ErrConfigLoad) {
		t.Errorf("error should wrap ErrConfigLoad, got: %v", err)
	}
}

// TestValidate_ZeroValues tests that the validate function catches zero/empty
// values directly, bypassing defaults. This verifies the safety net for
// programmatic construction of Config values.
func TestValidate_ZeroValues(t *testing.T) {
	tests := []struct {
		name       string
		cfg        *Config
		wantSubstr string
	}{
		{
			name:       "zero server ports",
			cfg:        &Config{},
			wantSubstr: "server.tesla_port",
		},
		{
			name: "zero max_conns",
			cfg: &Config{
				server: ServerConfig{TeslaPort: 443, ClientPort: 8080, MetricsPort: 9090},
			},
			wantSubstr: "database.max_conns",
		},
		{
			name: "zero telemetry fields",
			cfg: &Config{
				server:   ServerConfig{TeslaPort: 443, ClientPort: 8080, MetricsPort: 9090},
				database: DatabaseConfig{URL: "postgres://x", MaxConns: 10, MinConns: 2},
			},
			wantSubstr: "telemetry.max_vehicles",
		},
		{
			name: "zero drive durations",
			cfg: &Config{
				server:    ServerConfig{TeslaPort: 443, ClientPort: 8080, MetricsPort: 9090},
				database:  DatabaseConfig{URL: "postgres://x", MaxConns: 10, MinConns: 2},
				telemetry: TelemetryConfig{MaxVehicles: 100, EventBufferSize: 1000, BatchWriteInterval: 5 * time.Second, BatchWriteSize: 100},
			},
			wantSubstr: "drives.min_duration",
		},
		{
			name: "zero websocket fields",
			cfg: &Config{
				server:    ServerConfig{TeslaPort: 443, ClientPort: 8080, MetricsPort: 9090},
				database:  DatabaseConfig{URL: "postgres://x", MaxConns: 10, MinConns: 2},
				telemetry: TelemetryConfig{MaxVehicles: 100, EventBufferSize: 1000, BatchWriteInterval: 5 * time.Second, BatchWriteSize: 100},
				drives:    DrivesConfig{MinDuration: 2 * time.Minute, MinDistanceMiles: 0.1, EndDebounce: 30 * time.Second, GeocodeTimeout: 5 * time.Second},
			},
			wantSubstr: "websocket.heartbeat_interval",
		},
		{
			name: "empty auth secret",
			cfg: &Config{
				server:    ServerConfig{TeslaPort: 443, ClientPort: 8080, MetricsPort: 9090},
				database:  DatabaseConfig{URL: "postgres://x", MaxConns: 10, MinConns: 2},
				telemetry: TelemetryConfig{MaxVehicles: 100, EventBufferSize: 1000, BatchWriteInterval: 5 * time.Second, BatchWriteSize: 100},
				drives:    DrivesConfig{MinDuration: 2 * time.Minute, MinDistanceMiles: 0.1, EndDebounce: 30 * time.Second, GeocodeTimeout: 5 * time.Second},
				websocket: WebSocketConfig{HeartbeatInterval: 15 * time.Second, WriteTimeout: 10 * time.Second, MaxConnectionsPerUser: 5, ReadLimit: 4096},
			},
			wantSubstr: "auth.secret",
		},
		{
			name: "empty token_issuer",
			cfg: &Config{
				server:    ServerConfig{TeslaPort: 443, ClientPort: 8080, MetricsPort: 9090},
				database:  DatabaseConfig{URL: "postgres://x", MaxConns: 10, MinConns: 2},
				telemetry: TelemetryConfig{MaxVehicles: 100, EventBufferSize: 1000, BatchWriteInterval: 5 * time.Second, BatchWriteSize: 100},
				drives:    DrivesConfig{MinDuration: 2 * time.Minute, MinDistanceMiles: 0.1, EndDebounce: 30 * time.Second, GeocodeTimeout: 5 * time.Second},
				websocket: WebSocketConfig{HeartbeatInterval: 15 * time.Second, WriteTimeout: 10 * time.Second, MaxConnectionsPerUser: 5, ReadLimit: 4096},
				auth:      AuthConfig{Secret: "secret", TokenAudience: "aud"},
			},
			wantSubstr: "auth.token_issuer",
		},
		{
			name: "empty token_audience",
			cfg: &Config{
				server:    ServerConfig{TeslaPort: 443, ClientPort: 8080, MetricsPort: 9090},
				database:  DatabaseConfig{URL: "postgres://x", MaxConns: 10, MinConns: 2},
				telemetry: TelemetryConfig{MaxVehicles: 100, EventBufferSize: 1000, BatchWriteInterval: 5 * time.Second, BatchWriteSize: 100},
				drives:    DrivesConfig{MinDuration: 2 * time.Minute, MinDistanceMiles: 0.1, EndDebounce: 30 * time.Second, GeocodeTimeout: 5 * time.Second},
				websocket: WebSocketConfig{HeartbeatInterval: 15 * time.Second, WriteTimeout: 10 * time.Second, MaxConnectionsPerUser: 5, ReadLimit: 4096},
				auth:      AuthConfig{Secret: "secret", TokenIssuer: "iss"},
			},
			wantSubstr: "auth.token_audience",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validate(tt.cfg)
			if err == nil {
				t.Fatal("validate() expected error, got nil")
			}
			if !errors.Is(err, ErrInvalidValue) {
				t.Errorf("error should wrap ErrInvalidValue, got: %v", err)
			}
			if !strings.Contains(err.Error(), tt.wantSubstr) {
				t.Errorf("error %q should contain %q", err.Error(), tt.wantSubstr)
			}
		})
	}
}

func TestValidPort(t *testing.T) {
	tests := []struct {
		port int
		want bool
	}{
		{0, false},
		{-1, false},
		{1, true},
		{80, true},
		{443, true},
		{8080, true},
		{65535, true},
		{65536, false},
		{100000, false},
	}

	for _, tt := range tests {
		if got := validPort(tt.port); got != tt.want {
			t.Errorf("validPort(%d) = %v, want %v", tt.port, got, tt.want)
		}
	}
}
