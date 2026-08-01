package push

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// Content-state label bounds (MYR-172 review).
//
// schemas/live-activity.schema.json caps `destination` and `vehicleName` at 128
// characters. Nothing enforced it: the content-state copied the ride's dropoff
// label verbatim, and neither ride creation nor the vehicle-name endpoint bounds
// what it stores. Apple caps the whole Activity payload at 4KB and answers an
// over-cap push with a flat 400 — which takes out the WHOLE Activity for that
// ride, every subsequent ETA tick included, not merely the long field.

// TestTruncateLabelBoundary walks the exact edge, because off-by-one is the
// only interesting bug in a truncation.
func TestTruncateLabelBoundary(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantRunes int
		wantCut   bool
	}{
		{name: "empty", in: "", wantRunes: 0},
		{name: "short", in: "Home", wantRunes: 4},
		{
			name:      "exactly at the cap is untouched",
			in:        strings.Repeat("a", MaxContentStateLabel),
			wantRunes: MaxContentStateLabel,
		},
		{
			name:      "one over the cap is cut to the cap",
			in:        strings.Repeat("a", MaxContentStateLabel+1),
			wantRunes: MaxContentStateLabel,
			wantCut:   true,
		},
		{
			name:      "far over the cap",
			in:        strings.Repeat("a", 4096),
			wantRunes: MaxContentStateLabel,
			wantCut:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateLabel(tc.in)

			if n := utf8.RuneCountInString(got); n != tc.wantRunes {
				t.Errorf("rune count = %d, want %d", n, tc.wantRunes)
			}
			// The bound the schema promises is on the RESULT, ellipsis
			// included — a 128-rune cap that emits 129 is still drift.
			if n := utf8.RuneCountInString(got); n > MaxContentStateLabel {
				t.Errorf("result is %d runes, over the %d the schema declares", n, MaxContentStateLabel)
			}
			if cut := strings.HasSuffix(got, "…"); cut != tc.wantCut {
				t.Errorf("ellipsis present = %v, want %v", cut, tc.wantCut)
			}
		})
	}
}

// TestTruncateLabelIsRuneSafe pins the reason the cut counts runes rather than
// bytes: a byte slice through a multi-byte character yields a string that is
// not valid UTF-8, which encoding/json re-encodes as U+FFFD — so a destination
// in Japanese would arrive on the lock screen ending in a replacement
// character, and the contract's character cap would be a byte cap in disguise.
func TestTruncateLabelIsRuneSafe(t *testing.T) {
	// Each rune is 3 bytes, so a byte-based cut would land mid-character.
	long := strings.Repeat("東", MaxContentStateLabel+40)

	got := truncateLabel(long)

	if !utf8.ValidString(got) {
		t.Fatalf("truncated label is not valid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n != MaxContentStateLabel {
		t.Errorf("rune count = %d, want %d", n, MaxContentStateLabel)
	}
	if strings.ContainsRune(got, '�') {
		t.Error("truncated label contains U+FFFD; the cut split a multi-byte character")
	}
}

// TestContentStateBoundsBothLabels asserts the enforcement is at the
// content-state build — the one place every send path passes through, and the
// only one that also covers labels already in the database from before the
// bound existed.
func TestContentStateBoundsBothLabels(t *testing.T) {
	state, _ := contentState(RideContext{
		Status:      "enroute",
		VehicleName: strings.Repeat("v", 500),
		Destination: strings.Repeat("d", 500),
	}, ProgressAnchor{}, fixedNow)

	if n := utf8.RuneCountInString(state.Destination); n != MaxContentStateLabel {
		t.Errorf("destination = %d runes, want %d", n, MaxContentStateLabel)
	}
	if n := utf8.RuneCountInString(state.VehicleName); n != MaxContentStateLabel {
		t.Errorf("vehicleName = %d runes, want %d", n, MaxContentStateLabel)
	}
}

// TestActivityPayloadStaysUnderApplesCap is the assertion the bound exists for.
// Two unbounded labels were enough on their own to push a payload past 4KB;
// with the cap the worst case is comfortably inside it.
func TestActivityPayloadStaysUnderApplesCap(t *testing.T) {
	const applePayloadCap = 4096

	n := testActivityNotification()
	n.ContentState, _ = contentState(RideContext{
		Status: "enroute",
		// Multi-byte on purpose: the cap is in runes, so the worst case in
		// BYTES is the cap times the widest rune JSON will emit.
		VehicleName: strings.Repeat("東", 500),
		Destination: strings.Repeat("東", 500),
	}, ProgressAnchor{}, fixedNow)

	body, err := buildActivityPayload(n)
	if err != nil {
		t.Fatalf("buildActivityPayload() error = %v", err)
	}
	if len(body) >= applePayloadCap {
		t.Errorf("payload is %d bytes, at or over Apple's %d cap", len(body), applePayloadCap)
	}

	// And it is still decodable — a truncation that broke the JSON would be a
	// worse failure than the one it prevents.
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("payload does not decode: %v", err)
	}
}
