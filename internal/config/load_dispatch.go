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
	enabled, err := parseKillSwitchEnv("DISPATCH_ENABLED")
	if err != nil {
		return err
	}
	fc.dispatchEnabled = enabled

	// RESERVATION_DISPATCH_ENABLED is the MYR-179 scheduled-dispatch
	// kill-switch, deliberately SEPARATE from DISPATCH_ENABLED so the new
	// reservation machinery can be stopped without touching instant rides.
	// False stops the sweeper entirely: due reservations stay accepted,
	// latch-unclaimed and outcome-absent, so turning it back on picks up the
	// ones still inside their lateness window and honestly failing the rest,
	// instead of having burned them.
	reservation, err := parseKillSwitchEnv("RESERVATION_DISPATCH_ENABLED")
	if err != nil {
		return err
	}
	fc.reservationDispatchEnabled = reservation

	// DRIVE_RETENTION_PRUNER_ENABLED is the MYR-439 retention-sweep
	// kill-switch. It rides along in this loader because it is the same KIND of
	// thing — a background sweeper an operator must be able to stop without a
	// deploy — and the fail-fast parse above is exactly the property it needs:
	// a `DRIVE_RETENTION_PRUNER_ENABLED=off` typo silently reading as "on"
	// would be the good failure, but reading as "off" would silently suspend a
	// privacy commitment, so neither is acceptable and both are rejected.
	pruner, err := parseKillSwitchEnv("DRIVE_RETENTION_PRUNER_ENABLED")
	if err != nil {
		return err
	}
	fc.drivePrunerEnabled = pruner

	return nil
}

// parseKillSwitchEnv reads a boolean env var that defaults to ENABLED when
// unset, returning a descriptive ErrInvalidValue when it is set to something
// ParseBool rejects.
//
// Every switch this loads is a KILL-switch — the feature ships on and the
// variable exists to stop it without a deploy — so the default is fixed rather
// than a parameter. A future flag that must default OFF is a different thing
// (a feature gate, not a kill-switch) and should get its own helper rather
// than re-introducing an argument that only ever takes one value.
func parseKillSwitchEnv(name string) (bool, error) {
	v, ok := os.LookupEnv(name)
	if !ok {
		return true, nil
	}
	parsed, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("config.Load: %w: %s=%q is not a boolean", ErrInvalidValue, name, v)
	}
	return parsed, nil
}
