package config

import "time"

// Default values for optional configuration fields. These are production
// defaults and are applied before validation when a field is left at its
// zero value.

func applyServerDefaults(s *fileServerConfig) {
	if s.TeslaPort == 0 {
		s.TeslaPort = 443
	}
	if s.ClientPort == 0 {
		s.ClientPort = 8080
	}
	if s.MetricsPort == 0 {
		s.MetricsPort = 9090
	}
}

func applyDatabaseDefaults(d *fileDatabaseConfig) {
	if d.MaxConns == 0 {
		d.MaxConns = 20
	}
	if d.MinConns == 0 {
		d.MinConns = 5
	}
}

func applyTelemetryDefaults(t *fileTelemetryConfig) {
	if t.MaxVehicles == 0 {
		t.MaxVehicles = 100
	}
	if t.EventBufferSize == 0 {
		t.EventBufferSize = 1000
	}
	if t.BatchWriteInterval.Dur() == 0 {
		t.BatchWriteInterval = Duration{d: 5 * time.Second}
	}
	if t.BatchWriteSize == 0 {
		t.BatchWriteSize = 100
	}
}

func applyDrivesDefaults(d *fileDrivesConfig) {
	if d.MinDuration.Dur() == 0 {
		d.MinDuration = Duration{d: 2 * time.Minute}
	}
	if d.MinDistanceMiles == 0 {
		d.MinDistanceMiles = 0.1
	}
	if d.EndDebounce.Dur() == 0 {
		d.EndDebounce = Duration{d: 30 * time.Second}
	}
	if d.GeocodeTimeout.Dur() == 0 {
		d.GeocodeTimeout = Duration{d: 5 * time.Second}
	}
}

func applyWebSocketDefaults(ws *fileWebSocketConfig) {
	if ws.HeartbeatInterval.Dur() == 0 {
		ws.HeartbeatInterval = Duration{d: 15 * time.Second}
	}
	if ws.WriteTimeout.Dur() == 0 {
		ws.WriteTimeout = Duration{d: 10 * time.Second}
	}
	if ws.MaxConnectionsPerUser == 0 {
		ws.MaxConnectionsPerUser = 5
	}
	if ws.ReadLimit == 0 {
		ws.ReadLimit = 4096
	}
}

func applyAuthDefaults(a *fileAuthConfig) {
	if a.TokenIssuer == "" {
		a.TokenIssuer = "myrobotaxi"
	}
	if a.TokenAudience == "" {
		a.TokenAudience = "telemetry"
	}
}
