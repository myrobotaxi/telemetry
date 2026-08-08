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

// SkipErrorFor returns a *SkippedVehicleError when Tesla's push response
// lists vin under skipped_vehicles, and nil when the config actually applied.
// A nil result also yields nil — the transport error the caller already holds
// is the better diagnosis in that case.
func SkipErrorFor(result *FleetConfigResponse, vin string) error {
	if result == nil {
		return nil
	}
	reason, skipped := result.Response.SkippedVehicles[vin]
	if !skipped {
		return nil
	}
	return &SkippedVehicleError{RedactedVIN: redactVIN(vin), Reason: reason}
}
