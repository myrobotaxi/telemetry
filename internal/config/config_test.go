package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeTestConfig writes a JSON config file in dir and returns its path.
// It merges the provided overrides into a valid base config. Pass nil
// for a fully-defaulted config.
func writeTestConfig(t *testing.T, dir string, overrides map[string]any) string {
	t.Helper()

	base := map[string]any{
		"server": map[string]any{
			"tesla_port":   443,
			"client_port":  8080,
			"metrics_port": 9090,
		},
		"tls": map[string]any{
			"cert_file": "/certs/server.crt",
			"key_file":  "/certs/server.key",
			"ca_file":   "/certs/tesla-ca.pem",
		},
		"database": map[string]any{
			"max_conns": 20,
			"min_conns": 5,
		},
		"telemetry": map[string]any{
			"max_vehicles":         100,
			"event_buffer_size":    1000,
			"batch_write_interval": "5s",
			"batch_write_size":     100,
		},
		"drives": map[string]any{
			"min_duration":       "2m",
			"min_distance_miles": 0.1,
			"end_debounce":       "30s",
			"geocode_timeout":    "5s",
		},
		"websocket": map[string]any{
			"heartbeat_interval":       "15s",
			"write_timeout":            "10s",
			"max_connections_per_user":  5,
			"read_limit":               4096,
		},
		"auth": map[string]any{
			"token_issuer":   "myrobotaxi",
			"token_audience": "telemetry",
		},
	}

	for k, v := range overrides {
		base[k] = v
	}

	data, err := json.MarshalIndent(base, "", "  ")
	if err != nil {
		t.Fatalf("marshal test config: %v", err)
	}

	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write test config: %v", err)
	}
	return path
}

// setRequiredEnv sets the required env vars and returns a cleanup function.
func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/testdb")
	t.Setenv("AUTH_SECRET", "test-secret-key-at-least-32-chars-long")
}

func TestLoad_ValidConfig(t *testing.T) {
	dir := t.TempDir()
	path := writeTestConfig(t, dir, nil)
	setRequiredEnv(t)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	// Server
	if cfg.Server().TeslaPort != 443 {
		t.Errorf("Server().TeslaPort = %d, want 443", cfg.Server().TeslaPort)
	}
	if cfg.Server().ClientPort != 8080 {
		t.Errorf("Server().ClientPort = %d, want 8080", cfg.Server().ClientPort)
	}
	if cfg.Server().MetricsPort != 9090 {
		t.Errorf("Server().MetricsPort = %d, want 9090", cfg.Server().MetricsPort)
	}

	// TLS
	if cfg.TLS().CertFile != "/certs/server.crt" {
		t.Errorf("TLS().CertFile = %q, want %q", cfg.TLS().CertFile, "/certs/server.crt")
	}

	// Database
	wantURL := "postgres://user:pass@localhost:5432/testdb" //nolint:gosec // test credential
	if cfg.Database().URL != wantURL {
		t.Errorf("Database().URL = %q, want test URL", cfg.Database().URL)
	}
	if cfg.Database().MaxConns != 20 {
		t.Errorf("Database().MaxConns = %d, want 20", cfg.Database().MaxConns)
	}

	// Telemetry durations
	if cfg.Telemetry().BatchWriteInterval != 5*time.Second {
		t.Errorf("Telemetry().BatchWriteInterval = %v, want 5s", cfg.Telemetry().BatchWriteInterval)
	}

	// Drives
	if cfg.Drives().MinDuration != 2*time.Minute {
		t.Errorf("Drives().MinDuration = %v, want 2m", cfg.Drives().MinDuration)
	}
	if cfg.Drives().MinDistanceMiles != 0.1 {
		t.Errorf("Drives().MinDistanceMiles = %g, want 0.1", cfg.Drives().MinDistanceMiles)
	}

	// WebSocket
	if cfg.WebSocket().HeartbeatInterval != 15*time.Second {
		t.Errorf("WebSocket().HeartbeatInterval = %v, want 15s", cfg.WebSocket().HeartbeatInterval)
	}
	if cfg.WebSocket().ReadLimit != 4096 {
		t.Errorf("WebSocket().ReadLimit = %d, want 4096", cfg.WebSocket().ReadLimit)
	}

	// Auth
	if cfg.Auth().Secret != "test-secret-key-at-least-32-chars-long" {
		t.Errorf("Auth().Secret = %q, want test secret", cfg.Auth().Secret)
	}
	if cfg.Auth().TokenIssuer != "myrobotaxi" {
		t.Errorf("Auth().TokenIssuer = %q, want %q", cfg.Auth().TokenIssuer, "myrobotaxi")
	}
}

func TestLoad_DefaultsApplied(t *testing.T) {
	dir := t.TempDir()

	// Write a minimal config (empty JSON object) — all defaults should apply.
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	setRequiredEnv(t)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	tests := []struct {
		name string
		got  any
		want any
	}{
		{"server.tesla_port", cfg.Server().TeslaPort, 443},
		{"server.client_port", cfg.Server().ClientPort, 8080},
		{"server.metrics_port", cfg.Server().MetricsPort, 9090},
		{"database.max_conns", cfg.Database().MaxConns, 20},
		{"database.min_conns", cfg.Database().MinConns, 5},
		{"telemetry.max_vehicles", cfg.Telemetry().MaxVehicles, 100},
		{"telemetry.event_buffer_size", cfg.Telemetry().EventBufferSize, 1000},
		{"telemetry.batch_write_interval", cfg.Telemetry().BatchWriteInterval, 5 * time.Second},
		{"telemetry.batch_write_size", cfg.Telemetry().BatchWriteSize, 100},
		{"drives.min_duration", cfg.Drives().MinDuration, 2 * time.Minute},
		{"drives.min_distance_miles", cfg.Drives().MinDistanceMiles, 0.1},
		{"drives.end_debounce", cfg.Drives().EndDebounce, 30 * time.Second},
		{"drives.geocode_timeout", cfg.Drives().GeocodeTimeout, 5 * time.Second},
		{"websocket.heartbeat_interval", cfg.WebSocket().HeartbeatInterval, 15 * time.Second},
		{"websocket.write_timeout", cfg.WebSocket().WriteTimeout, 10 * time.Second},
		{"websocket.max_connections_per_user", cfg.WebSocket().MaxConnectionsPerUser, 5},
		{"websocket.read_limit", cfg.WebSocket().ReadLimit, int64(4096)},
		{"auth.token_issuer", cfg.Auth().TokenIssuer, "myrobotaxi"},
		{"auth.token_audience", cfg.Auth().TokenAudience, "telemetry"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("%s = %v, want %v", tt.name, tt.got, tt.want)
			}
		})
	}
}

func TestLoad_MissingRequiredEnvVars(t *testing.T) {
	tests := []struct {
		name       string
		setDBURL   bool
		setAuth    bool
		wantSubstr string
	}{
		{
			name:       "missing DATABASE_URL",
			setDBURL:   false,
			setAuth:    true,
			wantSubstr: "DATABASE_URL",
		},
		{
			name:       "missing AUTH_SECRET",
			setDBURL:   true,
			setAuth:    false,
			wantSubstr: "AUTH_SECRET",
		},
		{
			name:       "missing both",
			setDBURL:   false,
			setAuth:    false,
			wantSubstr: "DATABASE_URL",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTestConfig(t, dir, nil)

			// Clear env vars first by setting them to empty.
			t.Setenv("DATABASE_URL", "")
			t.Setenv("AUTH_SECRET", "")
			t.Setenv("MAPBOX_TOKEN", "")

			if tt.setDBURL {
				t.Setenv("DATABASE_URL", "postgres://test:test@localhost/db")
			}
			if tt.setAuth {
				t.Setenv("AUTH_SECRET", "test-secret")
			}

			_, err := Load(path)
			if err == nil {
				t.Fatal("Load() expected error, got nil")
			}
			if !errors.Is(err, ErrMissingRequired) {
				t.Errorf("error should wrap ErrMissingRequired, got: %v", err)
			}
		})
	}
}

func TestLoad_TLSEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	path := writeTestConfig(t, dir, nil)
	setRequiredEnv(t)

	t.Setenv("TLS_CERT_FILE", "/env/cert.pem")
	t.Setenv("TLS_KEY_FILE", "/env/key.pem")
	t.Setenv("TLS_CA_FILE", "/env/ca.pem")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if cfg.TLS().CertFile != "/env/cert.pem" {
		t.Errorf("TLS().CertFile = %q, want %q", cfg.TLS().CertFile, "/env/cert.pem")
	}
	if cfg.TLS().KeyFile != "/env/key.pem" {
		t.Errorf("TLS().KeyFile = %q, want %q", cfg.TLS().KeyFile, "/env/key.pem")
	}
	if cfg.TLS().CAFile != "/env/ca.pem" {
		t.Errorf("TLS().CAFile = %q, want %q", cfg.TLS().CAFile, "/env/ca.pem")
	}
}

func TestLoad_WebSocketAllowedOriginsEnvOverride(t *testing.T) {
	tests := []struct {
		name     string
		fileVal  []string
		envValue string
		envSet   bool
		want     []string
	}{
		{
			name:    "JSON value used when env unset",
			fileVal: []string{"https://myrobotaxi.app"},
			envSet:  false,
			want:    []string{"https://myrobotaxi.app"},
		},
		{
			name:     "env override replaces JSON allow-list",
			fileVal:  []string{"https://myrobotaxi.app"},
			envValue: "https://staging.myrobotaxi.app, https://www.myrobotaxi.app",
			envSet:   true,
			want:     []string{"https://staging.myrobotaxi.app", "https://www.myrobotaxi.app"},
		},
		{
			name:     "env single entry, no comma",
			envValue: "https://myrobotaxi.app",
			envSet:   true,
			want:     []string{"https://myrobotaxi.app"},
		},
		{
			name:     "env empty signals fail-closed",
			fileVal:  []string{"https://myrobotaxi.app"},
			envValue: "",
			envSet:   true,
			want:     nil,
		},
		{
			name:     "env whitespace-only signals fail-closed",
			fileVal:  []string{"https://myrobotaxi.app"},
			envValue: "  ,  ",
			envSet:   true,
			want:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			overrides := map[string]any{}
			if tt.fileVal != nil {
				overrides["websocket"] = map[string]any{
					"heartbeat_interval":       "15s",
					"write_timeout":            "10s",
					"max_connections_per_user": 5,
					"read_limit":               4096,
					"allowed_origins":          tt.fileVal,
				}
			}
			path := writeTestConfig(t, dir, overrides)
			setRequiredEnv(t)
			if tt.envSet {
				t.Setenv("WEBSOCKET_ALLOWED_ORIGINS", tt.envValue)
			}

			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}

			got := cfg.WebSocket().AllowedOrigins
			if len(got) != len(tt.want) {
				t.Fatalf("AllowedOrigins len: got %d (%v), want %d (%v)", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("AllowedOrigins[%d]: got %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestLoad_MapboxTokenOptional(t *testing.T) {
	dir := t.TempDir()
	path := writeTestConfig(t, dir, nil)
	setRequiredEnv(t)
	t.Setenv("MAPBOX_TOKEN", "")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.MapboxToken() != "" {
		t.Errorf("MapboxToken() = %q, want empty", cfg.MapboxToken())
	}

	// Now set it.
	t.Setenv("MAPBOX_TOKEN", "pk.test123")
	cfg, err = Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.MapboxToken() != "pk.test123" {
		t.Errorf("MapboxToken() = %q, want %q", cfg.MapboxToken(), "pk.test123")
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	setRequiredEnv(t)

	_, err := Load("/nonexistent/path/config.json")
	if err == nil {
		t.Fatal("Load() expected error for nonexistent file, got nil")
	}
	if !errors.Is(err, ErrConfigLoad) {
		t.Errorf("error should wrap ErrConfigLoad, got: %v", err)
	}
}

func TestLoad_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{not valid json`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	setRequiredEnv(t)

	_, err := Load(path)
	if err == nil {
		t.Fatal("Load() expected error for invalid JSON, got nil")
	}
	if !errors.Is(err, ErrConfigLoad) {
		t.Errorf("error should wrap ErrConfigLoad, got: %v", err)
	}
}
