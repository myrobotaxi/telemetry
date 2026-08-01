// MYR-394 ride-position poll environment overrides. Kept in their own file,
// alongside load_dispatch.go and load_service_repoll.go, so each background
// loop's knobs live together and load.go stays under the file-size cap.

package config

import (
	"fmt"
	"os"
	"time"
)

// defaultRidePollInterval mirrors telemetry.defaultRidePollInterval, and the
// bounds below are this layer's alone. It is restated here rather than imported
// because internal/config must not depend on internal/telemetry (the dependency
// rule runs cmd → internal/* and config is read by everything). The telemetry
// package applies its own default to a non-positive value, so the two can only
// ever disagree about which of two identical numbers is used.
const defaultRidePollInterval = 25 * time.Second

// minRidePollInterval and maxRidePollInterval bound an operator-supplied
// cadence. Unlike the other duration knobs in this package, this one is
// RANGE-checked rather than merely positive-checked, because both ends of the
// range are genuinely dangerous rather than merely odd:
//
//   - Below the floor the poller stops being a background reader and becomes a
//     sustained request stream against Tesla per active ride. `1s` is a
//     plausible typo for `1m`, and it is the kind of typo that gets an app's
//     Fleet API access rate-limited before anyone notices.
//   - Above the ceiling the marker it produces is stale enough that showing it
//     as the car's position is arguably a worse lie than the stale-stream
//     defect MYR-394 exists to fix.
//
// Both are therefore startup failures, not warnings: a cadence outside this
// range means the operator intended something this loop should not do.
const (
	minRidePollInterval = 10 * time.Second
	maxRidePollInterval = 5 * time.Minute
)

// applyRidePollEnvOverrides reads the MYR-394 ride-position poll knobs.
//
// The kill-switch defaults to ON and FAILS FAST on a non-boolean value, exactly
// like the dispatch and re-poll switches: a typo such as `no` or `off` must not
// read as "polling enabled" and leave an operator believing they have stopped
// the Fleet API traffic (the MYR-176 lesson — a kill-switch you cannot trust is
// worse than none).
func applyRidePollEnvOverrides(fc *fileConfig) error {
	// RIDE_POSITION_POLL_ENABLED stops the poller entirely. Tracking then
	// degrades to its pre-MYR-394 behaviour — the last STREAMED fix, which for
	// a non-streaming car is exactly the stale marker this feature fixes — but
	// nothing breaks and nothing is lost permanently: re-enabling the switch
	// resumes polling for every ride still open, because the startup reconcile
	// rebuilds the poller set from the database.
	enabled, err := parseKillSwitchEnv("RIDE_POSITION_POLL_ENABLED")
	if err != nil {
		return err
	}
	fc.ridePollEnabled = enabled

	// RIDE_POSITION_POLL_INTERVAL tunes the cadence. It is the only lever on the
	// resulting Fleet API request rate — one vehicle_data GET per active ride
	// per interval, and exactly one: the poller holds the narrow
	// telemetry.ridePositionRefresher interface, which cannot reach the MYR-320
	// /release_notes enricher that makes the connectivity-edge refresh two GETs.
	// So it is worth being able to widen without a deploy.
	interval, err := parseDurationEnv(
		"RIDE_POSITION_POLL_INTERVAL", "RIDE_POSITION_POLL_ENABLED", defaultRidePollInterval)
	if err != nil {
		return err
	}
	if interval < minRidePollInterval || interval > maxRidePollInterval {
		return fmt.Errorf(
			"config.Load: %w: RIDE_POSITION_POLL_INTERVAL=%q must be between %s and %s",
			ErrInvalidValue, os.Getenv("RIDE_POSITION_POLL_INTERVAL"),
			minRidePollInterval, maxRidePollInterval)
	}
	fc.ridePollInterval = interval

	return nil
}
