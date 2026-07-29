package push

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestVehicleLabel(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "nickname passes through", in: "Blue Whale", want: "Blue Whale"},
		{name: "unnamed car falls back", in: "", want: fallbackVehicleName},
		{name: "whitespace-only falls back", in: "   ", want: fallbackVehicleName},
		{name: "long nickname is capped", in: strings.Repeat("N", maxNameRunes+10), want: strings.Repeat("N", maxNameRunes)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := vehicleLabel(tt.in); got != tt.want {
				t.Errorf("vehicleLabel(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestVehicleLabelFallbackReadsNaturally guards the phrasing choice: the
// generic label has to fit every sentence it is dropped into, or the fallback
// produces nonsense on exactly the notifications that matter most.
func TestVehicleLabelFallbackReadsNaturally(t *testing.T) {
	label := vehicleLabel("")
	declined, ok := statusAlert(statusDeclined, "")
	if !ok {
		t.Fatal("declined produced no alert")
	}
	for _, sentence := range []string{declined.title, dueAlert("").title} {
		if !strings.HasPrefix(sentence, label) {
			t.Errorf("sentence %q does not start with the fallback label %q", sentence, label)
		}
		if strings.Contains(sentence, "  ") {
			t.Errorf("sentence %q has a doubled space", sentence)
		}
	}
}

func TestDisplayName(t *testing.T) {
	long := strings.Repeat("é", maxNameRunes+5)

	tests := []struct {
		name string
		in   *string
		want string
	}{
		{name: "nil is anonymous", want: ""},
		{name: "empty is anonymous", in: strptr(""), want: ""},
		{name: "whitespace is anonymous", in: strptr("  \t "), want: ""},
		{name: "first name passes through", in: strptr("Ada"), want: "Ada"},
		{name: "surname is dropped", in: strptr("Ada Lovelace"), want: "Ada"},
		{name: "leading whitespace is collapsed", in: strptr("   Ada  Lovelace "), want: "Ada"},
		{name: "long name is capped", in: strptr(long), want: strings.Repeat("é", maxNameRunes)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := displayName(tt.in)
			if got != tt.want {
				t.Errorf("displayName() = %q, want %q", got, tt.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("displayName() = %q is not valid UTF-8 — a multi-byte rune was split", got)
			}
		})
	}
}

func TestStatusAlertSelectsOnlyNotifiableTransitions(t *testing.T) {
	notifiable := map[string]bool{
		statusAccepted: true,
		statusDeclined: true,
		statusArrived:  true,
	}
	// Every status in the go_ride_requests CHECK enum.
	all := []string{"requested", "accepted", "declined", "enroute", "arrived", "completed", "cancelled"}

	for _, status := range all {
		t.Run(status, func(t *testing.T) {
			a, ok := statusAlert(status, "Blue Whale")
			if ok != notifiable[status] {
				t.Fatalf("statusAlert(%q) notifies = %v, want %v", status, ok, notifiable[status])
			}
			if !ok {
				return
			}
			if a.title == "" || a.body == "" {
				t.Errorf("statusAlert(%q) = %+v, want both a title and a body", status, a)
			}
		})
	}
}
