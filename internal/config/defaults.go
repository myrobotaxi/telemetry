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
		// 60s default chosen to relieve Postgres write pressure (MYR-131).
		// NFR-3.11 cold-load freshness tolerates ≤60s staleness because
		// browsers open the live WS immediately after /snapshot and the
		// next telemetry frame snaps the UI to current.
		t.BatchWriteInterval = Duration{d: 60 * time.Second}
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
	if d.StallTimeout.Dur() == 0 {
		// 15m: long enough that a drive-thru / rail-crossing stop
		// doesn't split a real drive, short enough that a missed
		// gear=P frame closes the drive promptly (MYR-160).
		d.StallTimeout = Duration{d: 15 * time.Minute}
	}
	if d.MaxDriveDuration.Dur() == 0 {
		// 12h backstop — see DrivesConfig.MaxDriveDuration.
		d.MaxDriveDuration = Duration{d: 12 * time.Hour}
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

func applyIdentityDefaults(i *fileIdentityConfig) {
	if i.AccessTokenTTL.Dur() == 0 {
		// 1h access-token lifetime (ADR-001 §5, MYR-193). Short enough that a
		// leaked access token is bounded; the refresh token (sliding 90d)
		// carries the long-lived session.
		i.AccessTokenTTL = Duration{d: time.Hour}
	}
	if i.RefreshTokenTTL.Dur() == 0 {
		// 90d sliding refresh-family life — each rotation re-anchors it.
		i.RefreshTokenTTL = Duration{d: 90 * 24 * time.Hour}
	}
	if i.AuthRateLimitPerMinute == 0 {
		// Per-IP cap on /api/auth/*: brute-force protection on the
		// pre-authentication surface. 30/min ≈ 0.5 req/s sustained.
		i.AuthRateLimitPerMinute = 30
	}
	if i.AuthRateLimitBurst == 0 {
		i.AuthRateLimitBurst = 10
	}
}

func applyProxyDefaults(p *fileProxyConfig) {
	if p.FleetTelemetryPort == 0 {
		p.FleetTelemetryPort = 443
	}
}
