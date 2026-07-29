package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/myrobotaxi/telemetry/internal/wserrors"
)

// TestRejectionToken covers the allow-list itself: a KNOWN Tesla rejection
// prose collapses to its canonical token, and anything else collapses to "" so
// the caller keeps the generic message. The inputs are the shape the transport
// actually hands us — sanitizeReason output, i.e. lowercased and charset-
// filtered, carrying the tesla-http-proxy's own
// `car could not execute command: ` / `vcsec could not execute command: `
// prefix (vehicle-command@v0.4.1 pkg/vehicle/infotainment.go:42,
// pkg/protocol/error.go:177, surfaced by pkg/proxy/proxy.go:146).
func TestRejectionToken(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   string
	}{
		// The client's own case (MYR-329, Jul 28): climate refused while the
		// car sat in service mode, and the owner was left guessing at battery.
		{"in service", "car could not execute command: vehicle is in service", ReasonVehicleInService},
		{"service mode", "car could not execute command: service mode is active", ReasonVehicleInService},
		{"user not present", "car could not execute command: user not present", ReasonUserNotPresent},
		{"driver not present", "car could not execute command: driver not present in vehicle", ReasonUserNotPresent},
		{"requires acknowledgement (en-gb)", "car could not execute command: requires user acknowledgement", ReasonRequiresUserAck},
		{"requires acknowledgment (en-us)", "car could not execute command: requires user acknowledgment", ReasonRequiresUserAck},
		{"remote access disabled", "car could not execute command: remote access disabled", ReasonRemoteAccessDisabled},
		{"mobile access disabled", "the vehicle has turned off mobile access", ReasonRemoteAccessDisabled},
		{"busy", "car could not execute command: vehicle busy", ReasonVehicleBusy},
		{"low battery", "car could not execute command: battery too low", ReasonLowBattery},
		{"insufficient charge", "car could not execute command: insufficient charge", ReasonLowBattery},
		{"not enough power", "car could not execute command: not enough power", ReasonLowBattery},

		// Everything outside the allow-list stays generic. These are real
		// upstream strings we deliberately do NOT translate.
		{"empty", "", ""},
		{"unspecified", "car could not execute command: unspecified error", ""},
		{"already locked", "already_locked", ""},
		{"opaque proxy code", "invalid_command", ""},
		{"unknown prose", "car could not execute command: setchargingamps failed", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rejectionToken(tt.reason); got != tt.want {
				t.Errorf("rejectionToken(%q) = %q want %q", tt.reason, got, tt.want)
			}
		})
	}
}

// TestCommandFailedMessage proves the wire `message` is the generic sentence
// alone for an unknown reason and the generic sentence PLUS the canonical
// token for a known one. The token is what the iOS client matches on; the
// prose in front of it is for humans reading a 502 body.
func TestCommandFailedMessage(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   string
	}{
		{"unknown keeps generic", "already_locked", genericCommandFailedMessage},
		{"empty keeps generic", "", genericCommandFailedMessage},
		{"known appends token", "car could not execute command: vehicle is in service",
			genericCommandFailedMessage + ": " + ReasonVehicleInService},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := commandFailedMessage(tt.reason); got != tt.want {
				t.Errorf("commandFailedMessage(%q) = %q want %q", tt.reason, got, tt.want)
			}
		})
	}
}

// TestCommandFailedMessageNeverLeaksUpstreamProse is the security property the
// allow-list exists for: the message on the wire is assembled ONLY from our own
// constants, so no upstream substring — however sanitizeReason mangled it —
// can ride out to a client. Even an input that matches the allow-list
// contributes only the canonical token, never its own bytes.
func TestCommandFailedMessageNeverLeaksUpstreamProse(t *testing.T) {
	hostile := []string{
		"car could not execute command: vehicle is in service at 37.7871,-122.3971",
		"car could not execute command: user not present 5yj3e1ea8kf000316",
		"vehicle is in service see https://maps.example.com/?q=37.7,-122.4",
		"service mode bearer eyjhbgcioijiuzi1niisinr5cci6ikpxvcj9",
	}
	for _, raw := range hostile {
		t.Run(raw, func(t *testing.T) {
			msg := commandFailedMessage(sanitizeReason(raw))
			if !isAssembledFromKnownParts(msg) {
				t.Errorf("message %q is not assembled purely from known constants", msg)
			}
		})
	}
}

// isAssembledFromKnownParts reports whether msg is exactly the generic
// sentence, or the generic sentence plus ": " plus one canonical token.
func isAssembledFromKnownParts(msg string) bool {
	if msg == genericCommandFailedMessage {
		return true
	}
	suffix, ok := strings.CutPrefix(msg, genericCommandFailedMessage+": ")
	if !ok {
		return false
	}
	for _, token := range KnownRejectionReasons() {
		if suffix == token {
			return true
		}
	}
	return false
}

// TestKnownRejectionReasonsAreStableTokens guards the shape of the contract
// the iOS client matches on: distinct, lowercase snake_case identifiers that
// survive sanitizeReason unchanged (so a token can never be mangled in transit)
// and that cannot be produced by prose accidentally.
func TestKnownRejectionReasonsAreStableTokens(t *testing.T) {
	seen := map[string]bool{}
	for _, token := range KnownRejectionReasons() {
		if token == "" {
			t.Fatal("empty token in the allow-list")
		}
		if seen[token] {
			t.Errorf("duplicate token %q", token)
		}
		seen[token] = true
		if got := sanitizeReason(token); got != token {
			t.Errorf("token %q does not survive sanitizeReason (got %q)", token, got)
		}
		if !strings.Contains(token, "_") {
			t.Errorf("token %q should be snake_case so it cannot occur in prose", token)
		}
	}
}

// TestExecute_CommandFailedCarriesKnownReasonInMessage is the end-to-end proof
// through the real Executor: an OutcomeFailed carrying an allow-listed reason
// keeps the command_failed code + 502 status and the sanitized raw reason on
// Detail (unchanged, for the server-side log), and now ALSO names the reason in
// Message — the field the REST handler writes to the wire.
func TestExecute_CommandFailedCarriesKnownReasonInMessage(t *testing.T) {
	tests := []struct {
		name        string
		reason      string
		wantMessage string
	}{
		{
			name:        "in service names the reason",
			reason:      "car could not execute command: vehicle is in service",
			wantMessage: genericCommandFailedMessage + ": " + ReasonVehicleInService,
		},
		{
			name:        "unknown stays generic",
			reason:      "car could not execute command: unspecified error",
			wantMessage: genericCommandFailedMessage,
		},
		{
			name:        "no reason at all stays generic",
			reason:      "",
			wantMessage: genericCommandFailedMessage,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := &fakeTransport{enabled: true, results: []TransportResult{{Outcome: OutcomeFailed, Reason: tt.reason}}}
			e := newExec(tr)
			_, err := e.Execute(context.Background(), Request{VIN: "V", Command: "auto_conditioning_stop", Scopes: grantAll()})

			var cErr *CommandError
			if !asCommandError(err, &cErr) {
				t.Fatalf("error is not *CommandError: %v", err)
			}
			if cErr.Code != wserrors.ErrCodeCommandFailed {
				t.Errorf("code = %q want %q", cErr.Code, wserrors.ErrCodeCommandFailed)
			}
			if cErr.Status != 502 {
				t.Errorf("status = %d want 502", cErr.Status)
			}
			if !cErr.Retryable {
				t.Error("command_failed should stay retryable")
			}
			if cErr.Message != tt.wantMessage {
				t.Errorf("message = %q want %q", cErr.Message, tt.wantMessage)
			}
			// Detail is untouched by this issue: the dispatch outcome log
			// still gets the full sanitized reason, not the collapsed token.
			if cErr.Detail != tt.reason {
				t.Errorf("detail = %q want %q", cErr.Detail, tt.reason)
			}
		})
	}
}

// TestExecute_NonCommandFailedOutcomesKeepTheirMessages proves the change is
// scoped to the command_failed rejection branch. The other terminal outcomes
// already have their own precise, actionable copy and a counter error is a
// signing-session problem rather than a car rejection — none of them should
// grow a reason token.
func TestExecute_NonCommandFailedOutcomesKeepTheirMessages(t *testing.T) {
	inService := "car could not execute command: vehicle is in service"
	tests := []struct {
		name    string
		outcome Outcome
		want    string
	}{
		{"not paired", OutcomeNotPaired, "virtual key not paired with vehicle"},
		{"permission denied", OutcomePermissionDenied, "Tesla rejected command: insufficient access"},
		{"invalid request", OutcomeInvalidRequest, "Tesla rejected command parameters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := &fakeTransport{enabled: true, results: []TransportResult{{Outcome: tt.outcome, Reason: inService}}}
			e := newExec(tr)
			_, err := e.Execute(context.Background(), Request{VIN: "V", Command: "door_lock", Scopes: grantAll()})
			var cErr *CommandError
			if !asCommandError(err, &cErr) {
				t.Fatalf("error is not *CommandError: %v", err)
			}
			if cErr.Message != tt.want {
				t.Errorf("message = %q want %q", cErr.Message, tt.want)
			}
		})
	}

	t.Run("counter error", func(t *testing.T) {
		tr := &fakeTransport{enabled: true, results: []TransportResult{
			{Outcome: OutcomeCounterError, Reason: inService},
			{Outcome: OutcomeCounterError, Reason: inService},
		}}
		e := newExec(tr)
		_, err := e.Execute(context.Background(), Request{VIN: "V", Command: "door_lock", Scopes: grantAll()})
		var cErr *CommandError
		if !asCommandError(err, &cErr) {
			t.Fatalf("error is not *CommandError: %v", err)
		}
		if cErr.Message != "signing session error after re-handshake" {
			t.Errorf("message = %q want the signing-session copy", cErr.Message)
		}
	})
}
