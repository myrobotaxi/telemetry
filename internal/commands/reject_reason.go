package commands

import "strings"

// This file owns ONE question: when the car says no, can we name why?
//
// MYR-329. A `command_failed` (502) means we reached the vehicle and it
// refused. Until now the wire message was the bare sentence "vehicle command
// failed", so the owner's app could only say "The car didn't accept that" —
// which on Jul 28 left the client guessing at his battery level when the real
// cause was that the car was sitting in service mode.
//
// The reason was already in the building. The tesla-http-proxy relays the
// car's own refusal as a nominal error:
//
//	{"response":{"result":false,"reason":"car could not execute command: <car prose>"}}
//
// (teslamotors/vehicle-command@v0.4.1 — pkg/vehicle/infotainment.go:37-42 reads
// ActionStatus.ResultReason.PlainText off the infotainment response and wraps
// it as a protocol.NominalError; pkg/protocol/error.go:177 does the same for
// VCSEC with a "vcsec could not execute command: " prefix; pkg/proxy/proxy.go:146
// serializes any nominal error into the carResponse.reason field above.)
// classifyResponse already parses that field into TransportResult.Reason and
// sanitizeReason already strips URLs/coordinates from it, but the Executor only
// ever put it on CommandError.Detail — a server-side-log field. The wire
// message stayed generic and the owner learned nothing.
//
// The car prose is FIRMWARE text: unstable, untranslated, and not a contract.
// So we do not forward it. We match it against a closed allow-list and emit our
// OWN canonical token, exactly the keyword-matching approach classifyResponse
// already uses one layer down for the same reason ("the proxy relays Tesla's
// own (unstable) prose and there is no stable machine code for these
// conditions"). Two properties fall out of that, and both matter:
//
//  1. Nothing upstream can reach a client. The message is assembled purely
//     from constants in this file, so an unrecognized — or hostile — reason
//     contributes zero bytes to the wire and the owner sees today's generic
//     copy. Naming a cause we are not sure of would be worse than saying
//     nothing.
//  2. The token, not the prose, is the client contract. Tesla can reword
//     "vehicle is in service" freely; we absorb it here by adding a keyword,
//     and no client ships a change.

// Canonical rejection reasons. These strings ARE the client contract — the iOS
// notice copy is keyed off them (MYR-329) — so treat them as append-only:
// rewording one silently reverts an owner to the generic message.
//
// They are deliberately snake_case identifiers rather than prose. Prose cannot
// accidentally contain one, and they survive sanitizeReason byte-for-byte, so a
// token can never be mangled between here and the wire.
const (
	// ReasonVehicleInService — the car is in service mode. Tesla refuses most
	// remote commands outright while a service visit is open. This is the
	// client's own Jul 28 case and the reason this issue exists.
	ReasonVehicleInService = "vehicle_in_service"
	// ReasonUserNotPresent — the command needs someone in the driver's seat
	// (Tesla guards several actuations this way).
	ReasonUserNotPresent = "user_not_present"
	// ReasonRequiresUserAck — the car wants a confirmation on its own
	// touchscreen before it will act.
	ReasonRequiresUserAck = "requires_user_acknowledgement"
	// ReasonRemoteAccessDisabled — remote/mobile access is switched off in the
	// car's own settings, so no amount of retrying will help. Mirrors
	// MESSAGEFAULT_ERROR_REMOTE_ACCESS_DISABLED.
	ReasonRemoteAccessDisabled = "remote_access_disabled"
	// ReasonVehicleBusy — the car is mid-something and declined to queue.
	// Mirrors MESSAGEFAULT_ERROR_BUSY. Genuinely transient.
	ReasonVehicleBusy = "vehicle_busy"
	// ReasonLowBattery — the pack is too low for the requested action. The
	// client GUESSED this on Jul 28 and was wrong; when it is actually true we
	// should be the ones to say so.
	ReasonLowBattery = "low_battery"
)

// genericCommandFailedMessage is the message a `command_failed` carries when
// the reason is absent or unrecognized — i.e. today's behavior, unchanged.
const genericCommandFailedMessage = "vehicle command failed"

// rejectionRule maps upstream keywords to one canonical token. Keywords are
// matched as substrings against the already-sanitized (lowercased) reason.
//
// Rules are evaluated in order, so the more specific phrasings sit ahead of the
// looser ones. Keep every keyword narrow enough that it cannot fire on an
// unrelated refusal: a wrong reason is worse than a generic one, because the
// owner acts on it.
type rejectionRule struct {
	token    string
	keywords []string
}

var rejectionRules = []rejectionRule{
	{ReasonVehicleInService, []string{"in service", "service mode"}},
	{ReasonRequiresUserAck, []string{
		"requires user acknowledg", // covers -ement and -ment
		"user acknowledgement", "user acknowledgment",
		"confirm on the touchscreen",
	}},
	{ReasonUserNotPresent, []string{
		"user not present", "driver not present", "no user present",
		"user is not present", "driver is not present",
	}},
	{ReasonRemoteAccessDisabled, []string{
		"remote access disabled", "remote access is disabled",
		"turned off remote access", "mobile access disabled",
		"turned off mobile access",
	}},
	{ReasonLowBattery, []string{
		"low battery", "battery too low", "battery level too low",
		"insufficient charge", "not enough power", "state of charge too low",
	}},
	// Last: "busy" is the loosest keyword in the table, so every more
	// specific rule gets first refusal on the string.
	{ReasonVehicleBusy, []string{"vehicle busy", "vehicle is busy", "car is busy"}},
}

// KnownRejectionReasons returns the canonical tokens, in table order. It exists
// so tests (and any future doc generation) enumerate the allow-list from the
// one place that defines it rather than restating it.
func KnownRejectionReasons() []string {
	tokens := make([]string, 0, len(rejectionRules))
	for _, rule := range rejectionRules {
		tokens = append(tokens, rule.token)
	}
	return tokens
}

// rejectionToken matches a sanitized upstream reason against the allow-list and
// returns the canonical token, or "" when the reason is absent or unrecognized.
func rejectionToken(reason string) string {
	if reason == "" {
		return ""
	}
	for _, rule := range rejectionRules {
		for _, keyword := range rule.keywords {
			if strings.Contains(reason, keyword) {
				return rule.token
			}
		}
	}
	return ""
}

// commandFailedMessage builds the wire `message` for a `command_failed`. A
// recognized reason is named after the generic sentence
// ("vehicle command failed: vehicle_in_service"); anything else yields the
// generic sentence alone, exactly as before this issue.
//
// The layout is deliberate: the leading sentence keeps the body readable for a
// human debugging a 502, and the trailing token is the stable thing a client
// matches on. A client that does not recognize the token MUST fall back to its
// generic copy — the set grows over time and old clients keep working.
func commandFailedMessage(reason string) string {
	token := rejectionToken(reason)
	if token == "" {
		return genericCommandFailedMessage
	}
	return genericCommandFailedMessage + ": " + token
}
