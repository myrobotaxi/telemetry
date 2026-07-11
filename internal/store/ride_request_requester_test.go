package store

import "testing"

// TestRequesterDisplayName pins the MYR-229 fallback chain: first name (first
// whitespace token of the display name) → email local-part → "Rider". The
// scanner only calls this for a rider that HAS a "User" row, so every case
// resolves to a non-empty value (the no-row omission is decided by the caller
// from requester_exists, not here).
func TestRequesterDisplayName(t *testing.T) {
	sp := func(s string) *string { return &s }

	tests := []struct {
		name  string
		disp  *string
		email *string
		want  string
	}{
		{
			name:  "display name yields first token",
			disp:  sp("Ada Lovelace"),
			email: sp("ada@example.com"),
			want:  "Ada",
		},
		{
			name:  "single-token display name",
			disp:  sp("Ada"),
			email: sp("ada@example.com"),
			want:  "Ada",
		},
		{
			name:  "leading and interior whitespace collapses to first token",
			disp:  sp("   Ada   Lovelace  "),
			email: sp("ada@example.com"),
			want:  "Ada",
		},
		{
			name:  "tab-separated display name",
			disp:  sp("Grace\tHopper"),
			email: nil,
			want:  "Grace",
		},
		{
			name:  "nil name falls back to email local-part",
			disp:  nil,
			email: sp("grace.hopper@navy.mil"),
			want:  "grace.hopper",
		},
		{
			name:  "empty name falls back to email local-part",
			disp:  sp(""),
			email: sp("katherine@nasa.gov"),
			want:  "katherine",
		},
		{
			name:  "whitespace-only name falls back to email local-part",
			disp:  sp("   "),
			email: sp("dorothy@nasa.gov"),
			want:  "dorothy",
		},
		{
			name:  "no usable name or email resolves to Rider literal",
			disp:  nil,
			email: nil,
			want:  "Rider",
		},
		{
			name:  "empty name and empty email resolves to Rider",
			disp:  sp(""),
			email: sp(""),
			want:  "Rider",
		},
		{
			name:  "email with empty local-part resolves to Rider",
			disp:  nil,
			email: sp("@example.com"),
			want:  "Rider",
		},
		{
			name:  "email with no at-sign resolves to Rider",
			disp:  nil,
			email: sp("not-an-email"),
			want:  "Rider",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := requesterDisplayName(tt.disp, tt.email)
			if got != tt.want {
				t.Fatalf("requesterDisplayName(%v, %v) = %q, want %q",
					deref(tt.disp), deref(tt.email), got, tt.want)
			}
		})
	}
}

func TestFirstNameToken(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "two tokens", in: "Ada Lovelace", want: "Ada"},
		{name: "surrounding whitespace collapses", in: "  Ada  Lovelace   ", want: "Ada"},
		{name: "single token", in: "Ada", want: "Ada"},
		{name: "empty", in: "", want: ""},
		{name: "whitespace only", in: "   ", want: ""},
		{name: "tab separator", in: "Grace\tHopper", want: "Grace"},
		{name: "hyphenated first token stays intact", in: "Jean-Luc Picard", want: "Jean-Luc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstNameToken(tt.in); got != tt.want {
				t.Errorf("firstNameToken(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestEmailLocalPart(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "simple local part", in: "ada@example.com", want: "ada"},
		{name: "dotted local part", in: "grace.hopper@x.io", want: "grace.hopper"},
		{name: "empty local part", in: "@example.com", want: ""},
		{name: "no at sign", in: "no-at-sign", want: ""},
		{name: "empty", in: "", want: ""},
		{name: "multiple at signs take first", in: "a@b@c", want: "a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := emailLocalPart(tt.in); got != tt.want {
				t.Errorf("emailLocalPart(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
