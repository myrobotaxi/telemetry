package store

import "testing"

// TestRequesterDisplayName pins the MYR-229 fallback chain: first name (first
// whitespace token of the display name) → email local-part → "Rider", with the
// field OMITTED (empty string) only when no "User" row exists.
func TestRequesterDisplayName(t *testing.T) {
	sp := func(s string) *string { return &s }

	tests := []struct {
		name  string
		found bool
		disp  *string
		email *string
		want  string
	}{
		{
			name:  "no user row omits the field",
			found: false,
			disp:  sp("Ada Lovelace"),
			email: sp("ada@example.com"),
			want:  "",
		},
		{
			name:  "display name yields first token",
			found: true,
			disp:  sp("Ada Lovelace"),
			email: sp("ada@example.com"),
			want:  "Ada",
		},
		{
			name:  "single-token display name",
			found: true,
			disp:  sp("Ada"),
			email: sp("ada@example.com"),
			want:  "Ada",
		},
		{
			name:  "leading and interior whitespace collapses to first token",
			found: true,
			disp:  sp("   Ada   Lovelace  "),
			email: sp("ada@example.com"),
			want:  "Ada",
		},
		{
			name:  "tab-separated display name",
			found: true,
			disp:  sp("Grace\tHopper"),
			email: nil,
			want:  "Grace",
		},
		{
			name:  "nil name falls back to email local-part",
			found: true,
			disp:  nil,
			email: sp("grace.hopper@navy.mil"),
			want:  "grace.hopper",
		},
		{
			name:  "empty name falls back to email local-part",
			found: true,
			disp:  sp(""),
			email: sp("katherine@nasa.gov"),
			want:  "katherine",
		},
		{
			name:  "whitespace-only name falls back to email local-part",
			found: true,
			disp:  sp("   "),
			email: sp("dorothy@nasa.gov"),
			want:  "dorothy",
		},
		{
			name:  "no usable name or email resolves to Rider literal",
			found: true,
			disp:  nil,
			email: nil,
			want:  "Rider",
		},
		{
			name:  "empty name and empty email resolves to Rider",
			found: true,
			disp:  sp(""),
			email: sp(""),
			want:  "Rider",
		},
		{
			name:  "email with empty local-part resolves to Rider",
			found: true,
			disp:  nil,
			email: sp("@example.com"),
			want:  "Rider",
		},
		{
			name:  "email with no at-sign resolves to Rider",
			found: true,
			disp:  nil,
			email: sp("not-an-email"),
			want:  "Rider",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := requesterDisplayName(tt.found, tt.disp, tt.email)
			if got != tt.want {
				t.Fatalf("requesterDisplayName(%v, %v, %v) = %q, want %q",
					tt.found, deref(tt.disp), deref(tt.email), got, tt.want)
			}
		})
	}
}

func TestFirstNameToken(t *testing.T) {
	tests := map[string]string{
		"Ada Lovelace":       "Ada",
		"  Ada  Lovelace   ": "Ada",
		"Ada":                "Ada",
		"":                   "",
		"   ":                "",
		"Grace\tHopper":      "Grace",
		"Jean-Luc Picard":    "Jean-Luc",
	}
	for in, want := range tests {
		if got := firstNameToken(in); got != want {
			t.Errorf("firstNameToken(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEmailLocalPart(t *testing.T) {
	tests := map[string]string{
		"ada@example.com":     "ada",
		"grace.hopper@x.io":   "grace.hopper",
		"@example.com":        "",
		"no-at-sign":          "",
		"":                    "",
		"a@b@c":               "a",
	}
	for in, want := range tests {
		if got := emailLocalPart(in); got != want {
			t.Errorf("emailLocalPart(%q) = %q, want %q", in, got, want)
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}
