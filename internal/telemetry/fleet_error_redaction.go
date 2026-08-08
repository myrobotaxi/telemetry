// VIN scrubbing for Tesla error text that reaches logs (MYR-448).
//
// FleetAPIError.Error() embeds Tesla's raw response body, and Tesla's 4xx
// bodies routinely echo the VIN that was asked about — as does the
// tesla-http-proxy when it reflects a request whose `vins` array is the
// plaintext VIN. The repo rule is last-4 only in production logs
// (data-classification §2.1).
//
// This was survivable while such errors appeared once per user request. The
// fleet-config reconciler turns it into a background loop that would reprint
// the same body every interval, per broken car, forever — so the scrub belongs
// on the path that logs it.

package telemetry

import (
	"regexp"
	"strings"
)

// vinPattern matches a 17-character Tesla VIN. The character class is the ISO
// 3779 alphabet: digits plus A-Z excluding I, O and Q, which are barred to
// avoid confusion with 1 and 0. Anchored on non-alphanumeric boundaries so it
// cannot bite a chunk out of a longer token (an access token, say).
var vinPattern = regexp.MustCompile(`(^|[^A-Za-z0-9])([A-HJ-NPR-Z0-9]{17})($|[^A-Za-z0-9])`)

// maxLoggedErrorText caps how much third-party error text reaches a log line.
// FleetAPIClient already limits a body to 64 KiB; that is still far too much
// to repeat on a loop, and the diagnostic value is all in the first line.
const maxLoggedErrorText = 512

// redactedErrorText renders err for logging with any embedded VIN reduced to
// its last four characters and the whole string length-capped.
//
// Returns "" for a nil error so a caller can log it unconditionally.
func redactedErrorText(err error) string {
	if err == nil {
		return ""
	}
	return redactVINsInText(err.Error())
}

// redactVINsInText replaces every VIN-shaped token in s with its redacted form
// and truncates the result.
func redactVINsInText(s string) string {
	s = vinPattern.ReplaceAllStringFunc(s, func(match string) string {
		loc := vinPattern.FindStringSubmatch(match)
		if len(loc) != 4 {
			return match
		}
		return loc[1] + redactVIN(loc[2]) + loc[3]
	})
	if len(s) > maxLoggedErrorText {
		s = s[:maxLoggedErrorText] + "…(truncated)"
	}
	return strings.TrimSpace(s)
}
