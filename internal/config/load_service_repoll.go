// MYR-320 periodic in-service re-poll environment overrides. Kept in their own
// file, alongside load_dispatch.go, so each background loop's knobs live
// together and load.go stays under the file-size cap.

package config

import (
	"fmt"
	"os"
	"time"
)

// defaultServiceRepollInterval mirrors telemetry.defaultRepollInterval. It is
// restated here rather than imported because internal/config must not depend on
// internal/telemetry (the dependency rule runs cmd → internal/* and config is
// read by everything). The telemetry package applies its own default to a
// non-positive value, so the two can only ever disagree about which of two
// identical numbers is used.
const defaultServiceRepollInterval = 15 * time.Minute

// applyServiceRepollEnvOverrides reads the MYR-320 in-service re-poll knobs.
//
// The kill-switch defaults to ON and FAILS FAST on a non-boolean value rather
// than silently falling back, exactly like the dispatch switches: a typo such as
// `no` or `off` must not read as "re-poll enabled" and leave an operator
// believing they have stopped the Fleet API traffic (the MYR-176 lesson — a
// kill-switch you cannot trust is worse than none).
//
// The interval fails fast the same way. An unparseable duration silently
// becoming 15 minutes would hide the typo for as long as nobody compared the
// logs against the intended value.
func applyServiceRepollEnvOverrides(fc *fileConfig) error {
	// SERVICE_REPOLL_ENABLED stops the periodic pass entirely. The monitor then
	// degrades to its pre-MYR-320 behaviour — reads fire on connectivity /
	// ServiceMode EDGES only — which is a safe place to stand while an operator
	// investigates Fleet API rate limiting. Nothing is lost permanently: the
	// next edge, or re-enabling the switch, re-reads everything.
	enabled, err := parseBoolEnv("SERVICE_REPOLL_ENABLED", true)
	if err != nil {
		return err
	}
	fc.serviceRepollEnabled = enabled

	// SERVICE_REPOLL_INTERVAL tunes the cadence. It bounds how stale an
	// in-service car's service window and REST-only detail fields can get, and
	// it is the only lever on the resulting Fleet API request rate (4 GETs per
	// in-service car per pass), so it is worth being able to widen without a
	// deploy.
	interval, err := parseDurationEnv("SERVICE_REPOLL_INTERVAL", defaultServiceRepollInterval)
	if err != nil {
		return err
	}
	fc.serviceRepollInterval = interval

	return nil
}

// parseDurationEnv reads a Go duration env var (e.g. "15m", "90s"), returning
// def when unset and a descriptive ErrInvalidValue when set to something
// time.ParseDuration rejects OR to a non-positive value. A zero or negative
// cadence is rejected rather than clamped: it means the operator intended
// something the loop cannot express, and silently substituting a default would
// hide that.
func parseDurationEnv(name string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(name)
	if !ok {
		return def, nil
	}
	parsed, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("config.Load: %w: %s=%q is not a duration", ErrInvalidValue, name, v)
	}
	if parsed <= 0 {
		return 0, fmt.Errorf(
			"config.Load: %w: %s=%q must be positive (set SERVICE_REPOLL_ENABLED=false to disable)",
			ErrInvalidValue, name, v)
	}
	return parsed, nil
}
