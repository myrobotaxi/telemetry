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

	// RIDE_RETENTION_PRUNER_ENABLED is the MYR-447 ride-retention sweep
	// kill-switch. Separate from the drive switch above even though the two
	// sweeps now share an engine: they delete from different tables, and an
	// operator stopping one because it is misbehaving must not thereby suspend
	// the other's privacy commitment. Same fail-fast parse, for the same reason
	// — an unparseable value here would silently decide whether a year-old ride
	// record and a removed feature's passenger data keep living.
	ridePruner, err := parseKillSwitchEnv("RIDE_RETENTION_PRUNER_ENABLED")
	if err != nil {
		return err
	}
	fc.ridePrunerEnabled = ridePruner

	// AUTO_ARRIVAL_ENABLED is the MYR-538 auto-arrival kill-switch. It belongs
	// beside the dispatch switches rather than the pruner ones because it gates
	// the same pipeline from the other end: RESERVATION_DISPATCH_ENABLED decides
	// whether a car is ever SENT to a pickup, this one whether the server may
	// notice it ARRIVED. False leaves the owner's "Picked up" tap as the only
	// writer of accepted → arrived, which is exactly the pre-MYR-538 behaviour
	// — nothing accumulates while it is off and nothing replays when it comes
	// back on.
	//
	// Same fail-fast parse as its neighbours, and here the argument is at its
	// sharpest: this is the only switch in the file that gates a feature which
	// writes a ride's lifecycle WITHOUT a person asking. An operator who typed
	// `AUTO_ARRIVAL_ENABLED=off` to stop it firing wrongly must not be left
	// believing they had.
	autoArrival, err := parseKillSwitchEnv("AUTO_ARRIVAL_ENABLED")
	if err != nil {
		return err
	}
	fc.autoArrivalEnabled = autoArrival

	// ARRIVAL_FLASH_ENABLED is the MYR-542 arrival-greeting kill-switch: three
	// headlight flashes when the car is observed at a waypoint. It sits after
	// AUTO_ARRIVAL_ENABLED because it is strictly downstream of it — the flash
	// consumes the `ride.waypoint_arrived` seam only auto-arrival publishes, so
	// turning auto-arrival off silences the flash too, while this switch stops
	// ONLY the flash and leaves the lifecycle write alone.
	//
	// Having its own switch matters precisely because of that asymmetry. This
	// is the one feature in the file that reaches out and MOVES A PHYSICAL CAR
	// on nobody's request; if it ever misfires — a wrong pickup coordinate, a
	// car flashing at 3am outside somebody's bedroom — the operator must be
	// able to stop the lights without also giving up the arrival detection that
	// is working fine. Same fail-fast parse as its neighbours.
	arrivalFlash, err := parseKillSwitchEnv("ARRIVAL_FLASH_ENABLED")
	if err != nil {
		return err
	}
	fc.arrivalFlashEnabled = arrivalFlash

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
