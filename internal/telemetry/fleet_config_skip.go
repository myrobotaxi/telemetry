// Typed detection of Tesla's "I accepted your request but did not apply it"
// response to a fleet-telemetry config push (MYR-448).
//
// THE TRAP THIS EXISTS TO CLOSE. `POST /api/1/vehicles/fleet_telemetry_config`
// answers HTTP 200 even when it applied the config to NO vehicle:
//
//	{"response":{"updated_vehicles":0,"skipped_vehicles":{"<vin>":"missing_key"}}}
//
// A caller that only checks the transport error therefore records a total
// no-op as a success. That is exactly how every external beta owner ended up
// linked-but-never-streaming: the one automatic push in the system fires
// during the OAuth callback, necessarily BEFORE the owner has paired the
// virtual key in the Tesla app, so Tesla skips it — silently.
//
// Everything that pushes a config MUST route the response through
// SkipErrorFor so a skip is an error, not a shrug.

package telemetry

import (
	"errors"
	"fmt"
	"strings"
)

// ErrVehicleSkipped is the sentinel behind every SkippedVehicleError, so
// callers can branch with errors.Is without depending on the concrete type.
var ErrVehicleSkipped = errors.New("fleet config not applied: vehicle skipped by Tesla")

// Tesla's skip reasons that mean "the owner has not finished pairing the
// virtual key". Two spellings are observed: `missing_key` is what the Fleet
// API returns in practice (docs/ops-cli.md §fleet-config push) and
// `not_paired` appears in Tesla's own examples and our handler fixtures.
// Both are the SAME recoverable condition — the owner has yet to approve the
// key at tesla.com/_ak/myrobotaxi.app — and both are healed by re-pushing
// once pairing lands, which is what the reconciler does.
const (
	SkipReasonMissingKey = "missing_key"
	SkipReasonNotPaired  = "not_paired"
)

// SkippedVehicleError reports that Tesla returned 200 but left the VIN
// unconfigured. Reason is Tesla's opaque per-VIN explanation, carried
// verbatim so operators see the real cause; the VIN is stored pre-redacted
// so the error string is always safe to log (data-classification §2.1).
type SkippedVehicleError struct {
	// RedactedVIN is the last-4 form produced by redactVIN — never a full VIN.
	RedactedVIN string
	// Reason is Tesla's skipped_vehicles map value (e.g. "missing_key").
	Reason string
}

func (e *SkippedVehicleError) Error() string {
	return fmt.Sprintf("fleet config not applied for vehicle %s: Tesla skipped it (reason=%q)",
		e.RedactedVIN, e.Reason)
}

// Unwrap ties the concrete error to the ErrVehicleSkipped sentinel.
func (e *SkippedVehicleError) Unwrap() error { return ErrVehicleSkipped }

// AwaitingVirtualKey reports whether the skip is the recoverable
// "owner has not paired the virtual key yet" case. Callers use this to pick
// the log level and the retry posture: true means keep trying (the owner can
// still fix it from the Tesla app), false means the cause is something we do
// not recognise and a human should look.
func (e *SkippedVehicleError) AwaitingVirtualKey() bool {
	return e.Reason == SkipReasonMissingKey || e.Reason == SkipReasonNotPaired
}

// SkipReasonNotApplied is synthesised when Tesla reports that it updated no
// vehicles but does not name ours in skipped_vehicles — see SkipErrorFor.
const SkipReasonNotApplied = "not_applied"

// SkipErrorFor returns a *SkippedVehicleError when Tesla did not apply the
// config to vin, and nil when it did. A nil result also yields nil — the
// transport error the caller already holds is the better diagnosis then.
//
// The question it answers is "did the config APPLY?", deliberately not "is my
// exact string a key in the map". Two ways the narrower question gets it
// wrong, both of which would silently recreate the MYR-448 bug one layer down:
//
//   - Key shape. Nothing in this codebase normalises VIN case, so the DB value
//     is whatever the web sync wrote, while the map key is whatever Tesla
//     echoes. An exact-match miss would be read as success.
//   - A skip Tesla declines to itemise. `updated_vehicles: 0` on a single-VIN
//     push means our car was not configured, whatever the map says.
//
// So the lookup is case-insensitive, and updated_vehicles == 0 is a backstop.
func SkipErrorFor(result *FleetConfigResponse, vin string) error {
	if result == nil {
		return nil
	}
	if reason, skipped := lookupSkipReason(result.Response.SkippedVehicles, vin); skipped {
		return &SkippedVehicleError{RedactedVIN: redactVIN(vin), Reason: reason}
	}
	// Nothing was updated, so nothing was applied — including ours.
	if result.Response.UpdatedVehicles == 0 {
		return &SkippedVehicleError{RedactedVIN: redactVIN(vin), Reason: SkipReasonNotApplied}
	}
	return nil
}

// lookupSkipReason finds vin in Tesla's skipped_vehicles map, tolerating case
// and surrounding-whitespace differences in the key.
func lookupSkipReason(skipped map[string]string, vin string) (string, bool) {
	if len(skipped) == 0 {
		return "", false
	}
	if reason, ok := skipped[vin]; ok {
		return reason, true
	}
	want := strings.ToUpper(strings.TrimSpace(vin))
	for k, reason := range skipped {
		if strings.ToUpper(strings.TrimSpace(k)) == want {
			return reason, true
		}
	}
	return "", false
}
