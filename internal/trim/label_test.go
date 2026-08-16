package trim

import "testing"

// The six production rows as of 2026-08-16, queried live during MYR-578's
// triage. These are the fixtures the feature exists for, verbatim — a resolver
// that is right in general and wrong on one of these is wrong.
func TestResolve_TheProductionFleet(t *testing.T) {
	tests := []struct {
		name        string
		model       string
		year        int
		storedLabel string
		badge       string
		vin17       string // digit 8 planted at index 7
		want        string
	}{
		{
			// trim=p100d, trim_label empty, VIN digit 8 = '6' (tri motor).
			// The client's directive AND Tesla's own filing agree: Plaid.
			name:  "Amruth's Model X Plaid",
			model: "Model X", year: 2026,
			storedLabel: "", badge: "p100d", vin17: "7SAXCDE6TF1234567",
			want: "Plaid",
		},
		{
			// Tesla's own label stands, verbatim — rung 1 outranks everything.
			name:  "Lunar (Model Y Performance)",
			model: "Model Y", year: 2026,
			storedLabel: "Performance", badge: "p74d", vin17: "7SAYGDETTF1234567",
			want: "Performance",
		},
		{
			name:  "Model 3 Performance",
			model: "Model 3", year: 2026,
			storedLabel: "Performance", badge: "p74d", vin17: "5YJ3E1ETTF1234567",
			want: "Performance",
		},
		{
			// trim_label "Base2024" is JUNK (digits) and must not leak; the
			// badge 62 on a 2026 car is the Standard's pack.
			name:  "Nabil's Model Y Standard",
			model: "Model Y", year: 2026,
			storedLabel: "Base2024", badge: "62", vin17: "7SAYGDEDTF1234567",
			want: "Standard",
		},
		{
			// No badge, no label, VIN digit 8 = 'A' (single motor): since the
			// 2026 lineup that is Standard OR Premium RWD, and naming one would
			// be a coin toss. NO TRIM is the only honest answer until the car
			// reports its badge.
			name:  "James's Model 3 (single motor, no badge yet)",
			model: "Model 3", year: 2026,
			storedLabel: "", badge: "", vin17: "5YJ3E1EATF1234567",
			want: "",
		},
		{
			name:  "Pearl (2025 Model 3, single motor, no badge)",
			model: "Model 3", year: 2025,
			storedLabel: "", badge: "", vin17: "5YJ3E1EASF1234567",
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Resolve(tt.model, tt.year, tt.storedLabel, tt.badge, tt.vin17)
			if got != tt.want {
				t.Fatalf("Resolve() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolve_BadgeTable(t *testing.T) {
	tests := []struct {
		name  string
		model string
		year  int
		badge string
		want  string
	}{
		{"P100D is Plaid whatever the case", "Model S", 2023, "P100D", "Plaid"},
		{"100D is the S/X Long Range", "Model X", 2024, "100d", "Long Range"},
		{"P74D is Performance", "Model Y", 2024, "P74D", "Performance"},
		{"74D pre-2026 is Long Range", "Model Y", 2024, "74D", "Long Range"},
		{"74D from the 2026 lineup is Premium", "Model Y", 2026, "74D", "Premium"},
		{"74 follows the same family", "Model 3", 2026, "74", "Premium"},
		{"62 on a 2026 car is Standard", "Model Y", 2026, "62", "Standard"},
		{"62 on the 2018-era Model 3 is Mid Range", "Model 3", 2019, "62", "Mid Range"},
		{"50 on 2022+ is RWD", "Model 3", 2023, "50", "RWD"},
		{"50 from the 2026 lineup is Standard", "Model 3", 2026, "50", "Standard"},
		{"an unknown badge maps to nothing", "Model Y", 2026, "97x", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fromBadge(tt.model, tt.year, tt.badge)
			if got != tt.want {
				t.Fatalf("fromBadge(%q) = %q, want %q", tt.badge, got, tt.want)
			}
		})
	}
}

func TestResolve_DriveUnitTable(t *testing.T) {
	vin := func(code byte) string {
		b := []byte("7SAXCDE_TF1234567")
		b[7] = code
		return string(b)
	}
	tests := []struct {
		name  string
		model string
		year  int
		code  byte
		want  string
	}{
		{"S/X tri motor is Plaid", "Model X", 2026, '6', "Plaid"},
		{"S/X dual motor is the base — no trim word", "Model X", 2026, '5', ""},
		{"Model 3 T is Performance", "Model 3", 2026, 'T', "Performance"},
		{"Model 3 C is the older Performance code", "Model 3", 2024, 'C', "Performance"},
		{"Model 3 dual standard pre-2026 is Long Range", "Model 3", 2024, 'B', "Long Range"},
		{"Model 3 dual standard 2026 is Premium", "Model 3", 2026, 'B', "Premium"},
		{"Model 3 single motor is deliberately unmapped", "Model 3", 2026, 'A', ""},
		{"Model Y T is Performance", "Model Y", 2026, 'T', "Performance"},
		{"Model Y F is the older Performance code", "Model Y", 2024, 'F', "Performance"},
		{"Model Y dual standard 2026 is Premium", "Model Y", 2026, 'E', "Premium"},
		{"Model Y single motor is deliberately unmapped", "Model Y", 2026, 'D', ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fromDriveUnit(tt.model, tt.year, vin(tt.code))
			if got != tt.want {
				t.Fatalf("fromDriveUnit(%c) = %q, want %q", tt.code, got, tt.want)
			}
		})
	}
}

func TestDisplaySafe_MirrorsTheClientFilter(t *testing.T) {
	tests := []struct {
		label string
		want  string
	}{
		{"Performance", "Performance"},
		{"Long Range", "Long Range"},
		{" Plaid ", "Plaid"},
		{"Base2024", ""},                        // digits — the junk that leaked as a caption
		{"p74d", ""},                            // a raw badge mis-routed into the label field
		{"Base", ""},                            // the absence of a trim spelled as a value
		{"base", ""},                            // …case-insensitively
		{"", ""},                                // absent
		{"Way Too Long A Trim Name Indeed", ""}, // over the shared 24-char cap
		{"Trim!", ""},                           // punctuation outside name characters
	}
	for _, tt := range tests {
		if got := displaySafe(tt.label); got != tt.want {
			t.Fatalf("displaySafe(%q) = %q, want %q", tt.label, got, tt.want)
		}
	}
}

// A malformed VIN contributes nothing — never a panic, never a guess.
func TestResolve_MalformedVINIsIgnored(t *testing.T) {
	for _, vin := range []string{"", "short", "7SAX"} {
		if got := Resolve("Model X", 2026, "", "", vin); got != "" {
			t.Fatalf("Resolve with vin %q = %q, want empty", vin, got)
		}
	}
}
