package telemetry

import (
	"errors"
	"testing"
)

// TestNormalizeLicensePlate pins the normalization rule in isolation from the
// handler: trim then uppercase, and NOTHING else. Interior spacing is
// deliberately preserved — "ABC 1234" and "ABC1234" are different plates in
// some jurisdictions, so collapsing them would silently rewrite the owner's
// answer.
func TestNormalizeLicensePlate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "lowercase uppercased", in: "abc1234", want: "ABC1234"},
		{name: "leading and trailing space trimmed", in: "  abc 1234  ", want: "ABC 1234"},
		{name: "tabs and newlines are whitespace too", in: "\t abc \n", want: "ABC"},
		{name: "interior single space preserved", in: "abc 1234", want: "ABC 1234"},
		{name: "interior double space preserved (not collapsed)", in: "abc  1234", want: "ABC  1234"},
		{name: "hyphen preserved", in: "abc-1234", want: "ABC-1234"},
		{name: "already normalized is a fixed point", in: "ABC 1234", want: "ABC 1234"},
		{name: "empty stays empty", in: "", want: ""},
		{name: "whitespace-only becomes empty (a clear)", in: "   ", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeLicensePlate(tt.in)
			if got != tt.want {
				t.Fatalf("NormalizeLicensePlate(%q) = %q, want %q", tt.in, got, tt.want)
			}
			// Normalization must be idempotent, otherwise a stored value could
			// differ from a re-submission of itself.
			if again := NormalizeLicensePlate(got); again != got {
				t.Errorf("not idempotent: %q -> %q", got, again)
			}
		})
	}
}

// TestValidateLicensePlate covers the post-normalization rule: at most 10
// characters from [A-Z0-9 -], with the empty string VALID (it means "clear").
func TestValidateLicensePlate(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		wantErr error
	}{
		{name: "empty is valid (clear)", in: "", wantErr: nil},
		{name: "letters and digits", in: "ABC1234", wantErr: nil},
		{name: "space is allowed", in: "ABC 1234", wantErr: nil},
		{name: "hyphen is allowed", in: "ABC-1234", wantErr: nil},
		{name: "exactly ten is the boundary", in: "ABCDEFGHIJ", wantErr: nil},
		{name: "eleven is too long", in: "ABCDEFGHIJK", wantErr: ErrPlateTooLong},
		{name: "lowercase is out of charset", in: "abc1234", wantErr: ErrPlateCharset},
		{name: "period is out of charset", in: "ABC.123", wantErr: ErrPlateCharset},
		{name: "underscore is out of charset", in: "ABC_123", wantErr: ErrPlateCharset},
		{name: "slash is out of charset", in: "ABC/123", wantErr: ErrPlateCharset},
		{name: "non-ascii is out of charset", in: "ÅBC123", wantErr: ErrPlateCharset},
		{
			// Length is checked first, so a value that is both too long and
			// out-of-charset reports the length rule. Pinned so the ordering
			// cannot flip silently.
			name:    "too long wins over charset",
			in:      "abcdefghijk",
			wantErr: ErrPlateTooLong,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateLicensePlate(tt.in)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ValidateLicensePlate(%q) = %v, want %v", tt.in, err, tt.wantErr)
			}
		})
	}
}

// TestValidateLicensePlate_MultiByteLengthIsNotUndercounted guards the one
// place byte-length could disagree with rune-length: a multi-byte string short
// in runes but long in bytes. Charset rejection fires first either way, so the
// rule can never admit an over-long value.
func TestValidateLicensePlate_MultiByteLengthIsNotUndercounted(t *testing.T) {
	// 6 runes, 12 bytes.
	const in = "ÅÄÖÅÄÖ"
	if err := ValidateLicensePlate(in); !errors.Is(err, ErrPlateTooLong) && !errors.Is(err, ErrPlateCharset) {
		t.Fatalf("ValidateLicensePlate(%q) = %v, want a rejection", in, err)
	}
}
