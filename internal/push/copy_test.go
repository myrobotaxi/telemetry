package push

import (
	"fmt"
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
	declined, ok := statusAlert(statusDeclined, "", false)
	if !ok {
		t.Fatal("declined produced no alert")
	}
	scheduledDeclined, ok := statusAlert(statusDeclined, "", true)
	if !ok {
		t.Fatal("scheduled declined produced no alert")
	}
	for _, sentence := range []string{declined.title, scheduledDeclined.title, dueAlert("").title} {
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
		for _, scheduled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/scheduled=%v", status, scheduled), func(t *testing.T) {
				a, ok := statusAlert(status, "Blue Whale", scheduled)
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
}

// TestStatusAlertDeclinedNamesTheReservation is the MYR-360 copy fix. An owner
// declining an ACCEPTED reservation is a new transition, and the pre-existing
// declined copy — "can't take this ride" — reads as a reply to a request the
// rider just made. A rider who booked a car for next Saturday and gets that on
// a lock screen has no way to tell it is about their CONFIRMED reservation.
//
// The scheduled variant names the scheduled ride and nothing more. It does NOT
// name the TIME: the server holds `scheduledFor` in UTC and knows no client
// time zone, so an absolute rendering here would be either wrong or unreadable
// — the same standing rule createdAlert follows.
func TestStatusAlertDeclinedNamesTheReservation(t *testing.T) {
	instant, ok := statusAlert(statusDeclined, "Blue Whale", false)
	if !ok {
		t.Fatal("instant declined produced no alert")
	}
	scheduled, ok := statusAlert(statusDeclined, "Blue Whale", true)
	if !ok {
		t.Fatal("scheduled declined produced no alert")
	}

	if instant.title == scheduled.title {
		t.Errorf("a declined reservation must not reuse the instant copy: both are %q", instant.title)
	}
	if !strings.Contains(scheduled.title, "scheduled") {
		t.Errorf("scheduled declined title %q does not tell the rider it is about their scheduled ride", scheduled.title)
	}
	if !strings.HasPrefix(scheduled.title, "Blue Whale") {
		t.Errorf("scheduled declined title %q must lead with the vehicle label", scheduled.title)
	}
	// The standing no-time rule: no digits may leak into the copy.
	for _, s := range []string{scheduled.title, scheduled.body} {
		if strings.ContainsAny(s, "0123456789") {
			t.Errorf("scheduled declined copy %q leaks a time/number; the server knows no client time zone", s)
		}
	}
	// Only `declined` forks on scheduling — accepted/arrived read identically
	// either way, so the fork stays as narrow as the defect it fixes.
	for _, status := range []string{statusAccepted, statusArrived} {
		a, _ := statusAlert(status, "Blue Whale", false)
		b, _ := statusAlert(status, "Blue Whale", true)
		if a != b {
			t.Errorf("statusAlert(%q) must not fork on scheduling: %+v vs %+v", status, a, b)
		}
	}
}
