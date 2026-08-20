package fleetorphan

// Ordering, redaction and the nil-logger sink. Split from sweep.go for the
// 300-line file cap; nothing here makes a decision.

import (
	"regexp"
	"sort"
	"strings"
)

// sortVINs orders candidates so the report is stable across runs. The VIN
// report inherits that order, since resolveVIN appends in sequence.
func sortVINs(c []candidate) {
	sort.Slice(c, func(i, j int) bool { return c[i].VIN < c[j].VIN })
}

// redactVIN shows only the last four characters (data-classification.md §2.1).
// The JSON report deliberately carries the full VIN in its own field; the logs
// do not.
func redactVIN(vin string) string {
	if len(vin) <= 4 {
		return vin
	}
	return "***" + vin[len(vin)-4:]
}

// vinPattern matches a 17-character Tesla VIN — the ISO 3779 alphabet (digits
// plus A-Z without I, O and Q), anchored on non-alphanumeric boundaries so it
// cannot bite a chunk out of a longer token. Copied from
// internal/telemetry/fleet_error_redaction.go, whose helper is unexported.
var vinPattern = regexp.MustCompile(`(^|[^A-Za-z0-9])([A-HJ-NPR-Z0-9]{17})($|[^A-Za-z0-9])`)

// maxErrorText caps third-party error text. The Fleet API client already limits
// a body to 64 KiB; that is still far more than belongs in a log line or a
// report field, and the diagnostic value is in the first line.
const maxErrorText = 512

// errText renders a third-party error for both the slog line and the report's
// Detail, with any embedded VIN reduced to its last four and the whole string
// length-capped.
//
// Tesla's 4xx bodies routinely echo the VIN that was asked about, so the raw
// text cannot go to a log. It is scrubbed in the REPORT too: the line already
// carries the VIN in its own field, so a second copy inside an error body adds
// nothing and only widens where the value can end up.
func errText(err error) string {
	if err == nil {
		return ""
	}
	s := vinPattern.ReplaceAllStringFunc(err.Error(), func(match string) string {
		g := vinPattern.FindStringSubmatch(match)
		if len(g) != 4 {
			return match
		}
		return g[1] + redactVIN(g[2]) + g[3]
	})
	if len(s) > maxErrorText {
		s = s[:maxErrorText] + "…(truncated)"
	}
	return strings.TrimSpace(s)
}

// discardWriter drops log output for the nil-logger default.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
