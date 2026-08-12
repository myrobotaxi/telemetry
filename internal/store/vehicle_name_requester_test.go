package store

import "testing"

// The MYR-535 requester-first-name ladder: name token → email local-part →
// "". The EMPTY terminal is the point of having a third ladder beside
// requesterDisplayName ("Rider") and firstNameFrom ("Owner"): the push copy
// has its own anonymous fallback title, and any literal here would be
// interpolated as a name — "Rider's pickup is coming up".
func TestRequesterFirstNameToken(t *testing.T) {
	s := func(v string) *string { return &v }
	tests := []struct {
		name  string
		nameP *string
		email *string
		want  string
	}{
		{name: "first token of the display name", nameP: s("Ada Lovelace"), email: s("ada@example.com"), want: "Ada"},
		{name: "email local-part when the name is empty", nameP: s("   "), email: s("ada@example.com"), want: "ada"},
		{name: "email local-part when the name is nil", email: s("ada@example.com"), want: "ada"},
		{name: "empty when nothing is usable", nameP: s(""), email: s("@example.com"), want: ""},
		{name: "empty when both are nil", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := requesterFirstNameToken(tt.nameP, tt.email); got != tt.want {
				t.Errorf("requesterFirstNameToken = %q, want %q", got, tt.want)
			}
		})
	}
}
