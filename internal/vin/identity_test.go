package vin

import "testing"

// The VINs below are SYNTHETIC. They reproduce the positional structure
// observed in production (WMI, position 4, position 10) without carrying any
// real vehicle's serial — the VIN is P1 under data-classification.md §1.3 and
// has no business being committed to a test file.
func TestModel(t *testing.T) {
	tests := []struct {
		name string
		vin  string
		want string
	}{
		{"Model X (position 4 'X')", "7SAXCDE2NTF000001", "Model X"},
		{"Model Y (position 4 'Y')", "7SAYGDEF9TF000002", "Model Y"},
		{"Model 3 (position 4 '3')", "5YJ3E1EA7TF000003", "Model 3"},
		{"Model S (position 4 'S')", "5YJSA1E20TF000004", "Model S"},
		{"Cybertruck (position 4 'C')", "7G2CEHED0TA000005", "Cybertruck"},
		{"Roadster (position 4 'R')", "5YJRE11B0TA000006", "Roadster"},
		{"lower-case VIN normalizes", "7saxcde2ntf000001", "Model X"},
		{"surrounding whitespace trims", " 7SAXCDE2NTF000001 ", "Model X"},

		// Never guess. Each of these must leave the caller with nothing to
		// write rather than a wrong model on a rider's ride card.
		{"unknown position-4 code yields nothing", "7SAQCDE2NTF000001", ""},
		{"too short yields nothing", "7SAX", ""},
		{"too long yields nothing", "7SAXCDE2NTF0000012", ""},
		{"empty yields nothing", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Model(tt.vin); got != tt.want {
				t.Errorf("Model(%q) = %q, want %q", tt.vin, got, tt.want)
			}
		})
	}
}

func TestModelYear(t *testing.T) {
	tests := []struct {
		name string
		vin  string
		want int
	}{
		// 'T' is the code every currently-linked production car carries, and
		// two of them independently hold `year` 2026 from the Prisma web-link
		// flow — this row is the assertion that the two sources agree.
		{"'T' is 2026", "7SAXCDE2NTF000001", 2026},
		{"'A' is the bottom of the window", "5YJ3E1EA7AF000003", 2010},
		{"'Y' is the top of the window", "5YJ3E1EA7YF000003", 2030},
		{"'N' is 2022", "5YJ3E1EA7NF000003", 2022},
		{"'P' skips over the excluded letters", "5YJ3E1EA7PF000003", 2023},
		{"lower-case VIN normalizes", "7saxcde2ntf000001", 2026},

		// ISO 3779 excludes these letters outright; a VIN carrying one is
		// malformed and must not be read as a year.
		{"'I' is excluded by the standard", "5YJ3E1EA7IF000003", 0},
		{"'O' is excluded by the standard", "5YJ3E1EA7OF000003", 0},
		{"'Q' is excluded by the standard", "5YJ3E1EA7QF000003", 0},
		{"'U' is excluded by the standard", "5YJ3E1EA7UF000003", 0},
		{"'Z' is excluded by the standard", "5YJ3E1EA7ZF000003", 0},

		// A digit means both 200x and 203x. Ambiguous until 2031, so it is
		// deliberately unmapped rather than guessed at.
		{"digit is ambiguous and yields nothing", "5YJ3E1EA79F000003", 0},

		{"too short yields nothing", "7SAX", 0},
		{"empty yields nothing", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ModelYear(tt.vin); got != tt.want {
				t.Errorf("ModelYear(%q) = %d, want %d", tt.vin, got, tt.want)
			}
		})
	}
}
