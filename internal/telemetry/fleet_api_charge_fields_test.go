package telemetry

import (
	"testing"
)

// TestDefaultFieldConfig_DetailedChargeStateResend is the MYR-333 guard.
//
// THE CLIENT-REPORTED DEFECT (Jul 29): a car being charged at a service centre
// showed no sign of charging in the app, while the very same screenshot showed
// the Charge tile reading "Port open" and the battery climbing 74% -> 76%. The
// data was flowing; the charging STATE was not.
//
// That asymmetry is the whole diagnosis, and it is a fleet-config asymmetry:
//
//   - FleetFieldChargePortDoorOpen (proto 183) carries a 120s resend (MYR-252),
//     so the open port re-asserts itself continuously and the app learned it.
//   - FleetFieldDetailedChargeState (proto 179) carried NO resend, and Tesla
//     emits it ON CHANGE ONLY. The transition to "Charging" fires exactly once,
//     when the technician plugs in. Any server that was not subscribed at that
//     instant — a reconnect, a deploy, a car that started charging while asleep,
//     or a fleet-config pushed after the session began — never hears the value
//     again for the entire charging session, and `chargeState` sits at its last
//     known value (commonly "Disconnected", or null on a car that has never
//     charged) until the session ENDS and the next transition fires.
//
// This is the identical failure class as MYR-300 (HvacPower: the car's own
// screen said "Cooling Down", the app said "Climate off") and MYR-299 (the seat
// coolers, where absence had to be made meaningful). The fix is the same one:
// a resend, so the value is re-asserted rather than latched.
//
// 120s is pinned to the cabin-comfort / media family and to the window
// defaultStreamFreshness (service_status_stream_freshness.go) is sized against,
// so the MYR-300 backfill gate stays coherent: a car that is genuinely
// streaming re-emits chargeState at least once per freshness window, which is
// what makes "we heard from the stream recently" a safe reason to drop the REST
// backfill's copy of the same field.
//
// The three charge-group SIBLINGS deliberately carry no resend and are asserted
// here as such, so a future edit has to think about the difference: chargeLevel
// (SOC), estimatedRange and timeToFull all CHANGE CONTINUOUSLY while a car is
// charging, so on-change emission re-delivers them by itself. chargeState is
// the one member of the group that latches.
func TestDefaultFieldConfig_DetailedChargeStateResend(t *testing.T) {
	t.Parallel()

	fields := DefaultFieldConfig()

	fc, ok := fields[FleetFieldDetailedChargeState]
	if !ok {
		t.Fatalf("field %q not found in default config", FleetFieldDetailedChargeState)
	}
	if fc.IntervalSeconds != 30 {
		t.Errorf("interval_seconds = %d, want 30 (unchanged battery/charging cadence)",
			fc.IntervalSeconds)
	}
	if fc.ResendIntervalSeconds == nil {
		t.Fatalf("field %q must set ResendIntervalSeconds — Tesla emits DetailedChargeState "+
			"on change only, so a server that missed the plug-in transition never learns the "+
			"car is charging for the whole session (MYR-333; same class as MYR-299/MYR-300)",
			FleetFieldDetailedChargeState)
	}
	if *fc.ResendIntervalSeconds != 120 {
		t.Errorf("resend_interval_seconds = %d, want 120 (matches defaultStreamFreshness and "+
			"the sibling ChargePortDoorOpen resend)", *fc.ResendIntervalSeconds)
	}
	// On-change semantics: a minimum-delta gate on an enum is meaningless and
	// Tesla rejects it for non-numeric fields.
	if fc.MinimumDelta != nil {
		t.Errorf("minimum_delta = %f, want nil (enum field, on-change semantics)", *fc.MinimumDelta)
	}
}

// TestDefaultFieldConfig_ChargeGroupSiblingsSelfRefresh documents WHY only one
// member of the `charge` atomic group needed the MYR-333 resend. These three
// are continuously-moving values during a charging session, so on-change
// emission already re-delivers them; adding a resend would spend vehicle-side
// buffer for nothing. If a future change gives one of them a resend, that is a
// deliberate decision that should update this test rather than slip in.
func TestDefaultFieldConfig_ChargeGroupSiblingsSelfRefresh(t *testing.T) {
	t.Parallel()

	fields := DefaultFieldConfig()

	for _, name := range []string{
		FleetFieldSOC,
		FleetFieldEstBatteryRange,
		FleetFieldTimeToFullCharge,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fc, ok := fields[name]
			if !ok {
				t.Fatalf("field %q not found in default config", name)
			}
			if fc.ResendIntervalSeconds != nil {
				t.Errorf("resend_interval_seconds = %d, want nil — this value moves "+
					"continuously while charging, so on-change emission refreshes it "+
					"without a resend (MYR-333)", *fc.ResendIntervalSeconds)
			}
		})
	}
}
