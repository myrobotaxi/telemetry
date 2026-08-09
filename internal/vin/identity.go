// Package vin derives a Tesla's MODEL and MODEL YEAR from its VIN (MYR-507).
//
// Why the VIN and not the Fleet API: Tesla's `vehicle_data` response — the one
// the MYR-320 enrichment already reads for `vehicle_config.exterior_color` —
// carries NO model year at any path. The VIN is the only source of a year the
// platform has, and once the year is coming from the VIN the model comes from
// the same 17 characters for free, so there is ONE derivation over ONE input
// rather than a year rule here and a `car_type` rule somewhere else.
//
// The VIN also has a property no API read has: it is in hand at LINK TIME, with
// no token, no awake car, and no network call. That is what the field report
// turned on — a car that had been in the fleet for hours still could not say
// what it was, because the enrichment that would have told us runs on a
// connectivity edge the car had never produced.
//
// Positional VIN semantics are ISO 3779 plus Tesla's own assignment of
// position 4. This package NEVER GUESSES: an unrecognised code yields the zero
// value ("" / 0), which every caller treats as "say nothing" rather than
// writing a wrong answer over a right one.
package vin

import "strings"

// Length is the fixed length of a road-vehicle VIN (ISO 3779). Anything else is
// not a VIN and gets the zero value from every function here.
const Length = 17

// modelByCode maps Tesla's VIN position 4 — the model line — to the display
// name the wire contract uses for `model` on both §7.0 and §7.1.
//
// Deliberately NOT exhaustive over everything Tesla has ever built. A code that
// is not listed returns "", which leaves the stored model alone; inventing a
// name for an unknown code would put a wrong model on a rider's ride card, which
// is strictly worse than the blank this issue exists to fix.
var modelByCode = map[byte]string{
	'S': "Model S",
	'3': "Model 3",
	'X': "Model X",
	'Y': "Model Y",
	'C': "Cybertruck",
	'R': "Roadster",
}

// yearByCode maps ISO 3779 position 10 — the model-year code — to a calendar
// year, over the 2010-2030 window.
//
// The letters I, O, Q, U and Z are excluded by the standard and are absent here.
// DIGITS ARE DELIBERATELY ABSENT TOO: in the 30-year cycle a digit means both
// 200x and 203x, and until 2031 the only real-world reading is 200x — a window
// in which Tesla shipped only the first-generation Roadster. Rather than encode
// an ambiguity nobody can act on, a digit returns 0 and the caller leaves the
// column alone. Extend this map in 2031, when a digit starts being unambiguous
// in the other direction.
var yearByCode = map[byte]int{
	'A': 2010, 'B': 2011, 'C': 2012, 'D': 2013, 'E': 2014,
	'F': 2015, 'G': 2016, 'H': 2017, 'J': 2018, 'K': 2019,
	'L': 2020, 'M': 2021, 'N': 2022, 'P': 2023, 'R': 2024,
	'S': 2025, 'T': 2026, 'V': 2027, 'W': 2028, 'X': 2029,
	'Y': 2030,
}

// Model returns the display model name encoded at VIN position 4, or "" when
// the VIN is malformed or the code is one this package does not recognise.
//
// Verified against every real vehicle in the production fleet: position 4 'X'
// on the Model X, 'Y' on the two Model Ys, '3' on the Model 3 — matching the
// `model` column the Prisma web-link flow independently wrote for the rows that
// have one.
func Model(v string) string {
	if !plausible(v) {
		return ""
	}
	return modelByCode[normalize(v)[3]]
}

// ModelYear returns the calendar model year encoded at VIN position 10, or 0
// when the VIN is malformed or the code is not in the 2010-2030 letter window.
//
// Verified against production: code 'T' on all three currently-linked cars,
// two of which independently carry `year` 2026 from the Prisma web-link flow.
func ModelYear(v string) int {
	if !plausible(v) {
		return 0
	}
	return yearByCode[normalize(v)[9]]
}

// plausible reports whether v is shaped like a VIN at all. Length is the only
// structural check: this package's job is to read two positions, and a check
// digit validation would reject legitimate VINs for reasons that have nothing
// to do with the two characters being read.
func plausible(v string) bool {
	return len(strings.TrimSpace(v)) == Length
}

// normalize upper-cases and trims so a lower-case or padded VIN from a fixture
// or a hand-built test row reads the same as the canonical form Tesla emits.
func normalize(v string) string {
	return strings.ToUpper(strings.TrimSpace(v))
}
