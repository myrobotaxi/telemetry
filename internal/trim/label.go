// Package trim resolves a vehicle's DISPLAY trim label ("Plaid",
// "Performance", "Premium", "Standard", "Long Range") from the three sources
// the platform holds, in strict precedence order (MYR-578).
//
// THE CLIENT'S ASK, verbatim: "p100d is plaid, let's create that mapping, also
// for James car vs Nabil's car we can't see if it's a Model Y Standard, Model Y
// Premium, Model 3 Premium, etc."
//
// WHY A RESOLVER RATHER THAN EMITTING THE STORED LABEL RAW. Tesla's
// vehicle_config gives us two trim facts per car and fills them unevenly:
// `performance_package` (the display label — "Performance") is EMPTY for half
// the fleet and junk ("Base2024") for some of the rest, while `trim_badging`
// (the raw badge — "p100d", "62") is filled more often but is an internal code
// no rider should read. Meanwhile the VIN's digit 8 encodes the DRIVE UNIT in
// Tesla's own NHTSA Part 565 filings — and a tri-motor Model X IS a Plaid, so
// for several trims the VIN alone is authoritative.
//
// THE LADDER, and why each rung outranks the next:
//
//  1. Tesla's OWN display label, when it is display-safe. Tesla naming its own
//     car outranks any inference of ours ("Performance" on Lunar stays exactly
//     as Tesla spelled it). "Display-safe" mirrors the iOS filter
//     (`VehicleTrimLabel`, MYR-507): letters and name punctuation only, ≤ 24
//     chars, and never a bare "Base" — so "Base2024" (digits) falls through
//     instead of leaking to a lock screen.
//  2. The BADGE code — the community-validated table (TeslaMate's, deployed
//     across a very large fleet) cross-checked against our own: both cars in
//     this fleet streaming P74D carry Tesla's label "Performance".
//  3. The VIN digit-8 DRIVE-UNIT code, per Tesla's published MY2023–MY2026
//     decoders — only the codes whose trim reading is unambiguous. A
//     single-motor Model 3 ('A') is deliberately NOT mapped: since the
//     2026 lineup both Standard and Premium RWD are single-motor, and naming
//     one would be a coin toss printed on a ride card.
//  4. Nothing. An unknown trim renders as no trim, never as a guess.
//
// MARKETING-NAME CHURN IS YEAR-GATED. Tesla renamed the Long Range family to
// "Premium" (and the base to "Standard") with the 2026-model-year lineup, so
// the same dual-motor-standard drive unit reads "Long Range" on a 2024 car and
// "Premium" on a 2026 one. The gate is the model YEAR — the fact the name
// change is actually keyed on — not the calendar.
package trim

import (
	"strings"
	"unicode"
)

// premiumEraYear is the first model year of the Standard/Premium lineup naming
// (announced with the 2026 model year). Before it, the dual-motor non-Performance
// trim is "Long Range".
const premiumEraYear = 2026

// maxLabelLength mirrors the iOS `VehicleTrimLabel.maxCharacters` — one limit,
// both sides, so the server can never emit a label the client refuses.
const maxLabelLength = 24

// Resolve returns the display trim label for a vehicle, or "" when none can be
// stated honestly. Inputs may be empty/nil-ish zero values; every rung degrades
// to the next.
//
//   - model: the display model line ("Model X"), as §7.0 emits it.
//   - year:  the model year (0 = unknown).
//   - storedLabel: Tesla's vehicle_config.performance_package, verbatim.
//   - badge: Tesla's vehicle_config.trim_badging, verbatim ("p100d").
//   - vin:   the 17-char VIN ("" or short = no VIN rung).
func Resolve(model string, year int, storedLabel, badge, vin string) string {
	if label := displaySafe(storedLabel); label != "" {
		return label
	}
	if label := fromBadge(model, year, badge); label != "" {
		return label
	}
	return fromDriveUnit(model, year, vin)
}

// displaySafe returns the stored label when it may be shown to a rider, else "".
// The rule mirrors the iOS `VehicleTrimLabel` filter (MYR-507): a trim is a
// short marketing word, so any digit ("Base2024", a raw "p74d" mis-routed
// here), any punctuation outside name characters, an over-long value, or a bare
// "Base" (the ABSENCE of a trim spelled as a value) is not one.
func displaySafe(label string) string {
	label = strings.TrimSpace(label)
	if label == "" || len(label) > maxLabelLength || strings.EqualFold(label, "base") {
		return ""
	}
	for _, r := range label {
		if !unicode.IsLetter(r) && r != ' ' && r != '-' && r != '+' {
			return ""
		}
	}
	return label
}

// fromBadge maps Tesla's trim_badging code to its marketing name.
//
// The table is TeslaMate's fleet-proven mapping plus this fleet's own
// validation (P74D ↔ "Performance", twice), with the 2026 renames year-gated:
//
//	P100D → Plaid          (S/X — the client's own directive, and the VIN
//	                        agrees: the one P100D car in this fleet is a
//	                        tri-motor X)
//	100D  → Long Range     (S/X dual)
//	P74D  → Performance    (3/Y)
//	74D/74→ Long Range,    Premium from the 2026 lineup
//	62    → Standard from the 2026 lineup; Mid Range on the 2018-era Model 3
//	        that badge originally belonged to
//	50    → Standard from the 2026 lineup; RWD on 2022+; Standard Range before
func fromBadge(model string, year int, badge string) string {
	switch strings.ToUpper(strings.TrimSpace(badge)) {
	case "P100D":
		return "Plaid"
	case "100D":
		return "Long Range"
	case "P74D":
		return "Performance"
	case "74D", "74":
		return longRangeName(year)
	case "62":
		if year > 0 && year < premiumEraYear && model == "Model 3" {
			return "Mid Range"
		}
		return "Standard"
	case "50":
		switch {
		case year >= premiumEraYear:
			return "Standard"
		case year >= 2022:
			return "RWD"
		case year > 0:
			return "Standard Range"
		default:
			return "Standard"
		}
	default:
		return ""
	}
}

// fromDriveUnit maps VIN digit 8 — the motor/drive-unit code in Tesla's own
// NHTSA Part 565 VIN decoder filings — to a trim name, for exactly the codes
// whose reading is unambiguous:
//
//	Model S/X:  6 = Tri Motor → Plaid (the tri-motor S/X IS the Plaid; the
//	            MY2025 filing spells the code, the marketing name follows by
//	            definition). 5 = Dual Motor is the BASE S/X, which carries no
//	            trim word at all — deliberately unmapped.
//	Model 3:    T = Dual Motor Performance (MY2025 filing, and both 'T' cars in
//	            this fleet carry Tesla's own "Performance" label);
//	            C = Dual Motor Performance (the MY2023/24 spelling);
//	            B = Dual Motor Standard → Long Range / Premium by year.
//	            A = Single Motor is UNMAPPED: from the 2026 lineup both
//	            Standard and Premium RWD are single-motor.
//	Model Y:    T = Performance (fleet-validated ×2, and the Model 3 filing
//	            uses the same letter for the same drive unit);
//	            F = Dual Motor Performance (MY2023/24 filing);
//	            E = Dual Motor Standard → Long Range / Premium by year.
//	            D = Single Motor is UNMAPPED for the Model 3 'A' reason.
func fromDriveUnit(model string, year int, vin string) string {
	vin = strings.ToUpper(strings.TrimSpace(vin))
	if len(vin) != 17 {
		return ""
	}
	code := vin[7]
	switch model {
	case "Model S", "Model X":
		if code == '6' {
			return "Plaid"
		}
	case "Model 3":
		switch code {
		case 'T', 'C':
			return "Performance"
		case 'B':
			return longRangeName(year)
		}
	case "Model Y":
		switch code {
		case 'T', 'F':
			return "Performance"
		case 'E':
			return longRangeName(year)
		}
	}
	return ""
}

// longRangeName is the dual-motor-standard trim's marketing name for a model
// year: "Premium" from the 2026 lineup, "Long Range" before it. An UNKNOWN year
// (0) reads as the current lineup — every car this platform provisions today is
// current-generation, and "Long Range" on a 2026 car is the wronger guess.
func longRangeName(year int) string {
	if year > 0 && year < premiumEraYear {
		return "Long Range"
	}
	return "Premium"
}
