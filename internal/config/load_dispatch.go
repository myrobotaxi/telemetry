// Nav-dispatch environment overrides. Extracted from load.go (MYR-179) so the
// two dispatch kill-switches live together and load.go stays under the
// file-size cap.

package config

import (
	"fmt"
	"os"
	"strconv"
)

// applyDispatchEnvOverrides reads the two nav-dispatch kill-switches. Both
// default to ON when unset and both FAIL FAST on a non-boolean value rather
// than silently falling back — a typo like `no` or `off` must not read as
// "dispatch enabled" and quietly leave the operator believing dispatch is
// stopped (the MYR-176 lesson: a kill-switch you cannot trust is worse than
// none).
//
// Accepted values are exactly strconv.ParseBool's:
// 1/t/T/TRUE/true/True/0/f/F/FALSE/false/False.
func applyDispatchEnvOverrides(fc *fileConfig) error {
	// DISPATCH_ENABLED is the MYR-176 nav-dispatch kill-switch: false records
	// every dispatch (instant OR reservation) as `skipped` with no Tesla call.
	enabled, err := parseBoolEnv("DISPATCH_ENABLED", true)
	if err != nil {
		return err
	}
	fc.dispatchEnabled = enabled

	// RESERVATION_DISPATCH_ENABLED is the MYR-179 scheduled-dispatch
	// kill-switch, deliberately SEPARATE from DISPATCH_ENABLED so the new
	// reservation machinery can be stopped without touching instant rides.
	// False stops the sweeper entirely: due reservations stay accepted,
	// latch-unclaimed and outcome-absent, so turning it back on picks up the
	// ones still inside their busy-hold window instead of having burned them.
	reservation, err := parseBoolEnv("RESERVATION_DISPATCH_ENABLED", true)
	if err != nil {
		return err
	}
	fc.reservationDispatchEnabled = reservation

	return nil
}

// parseBoolEnv reads a boolean env var, returning def when unset and a
// descriptive ErrInvalidValue when set to something ParseBool rejects.
func parseBoolEnv(name string, def bool) (bool, error) {
	v, ok := os.LookupEnv(name)
	if !ok {
		return def, nil
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("config.Load: %w: %s=%q is not a boolean", ErrInvalidValue, name, v)
	}
	return parsed, nil
}
