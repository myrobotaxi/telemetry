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
	declined, ok := statusAlert(statusDeclined, "", false, false)
	if !ok {
		t.Fatal("declined produced no alert")
	}
	scheduledDeclined, ok := statusAlert(statusDeclined, "", true, false)
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
				a, ok := statusAlert(status, "Blue Whale", scheduled, false)
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

// TestOwnerStatusAlertSelectsOnlyEnroute is the MYR-462 companion to the table
// above, and the two together are what keep the audiences from drifting: the
// owner's copy function must speak on `enroute` and stay silent on every other
// status, including the ones the RIDER's function speaks on. An owner does not
// need a banner telling them about their own accept, decline or arrival taps.
func TestOwnerStatusAlertSelectsOnlyEnroute(t *testing.T) {
	// Every status in the go_ride_requests CHECK enum.
	all := []string{"requested", "accepted", "declined", "enroute", "arrived", "completed", "cancelled"}

	for _, status := range all {
		t.Run(status, func(t *testing.T) {
			a, ok := ownerStatusAlert(status, nil, riderCancelNone)
			want := status == statusEnroute
			if ok != want {
				t.Fatalf("ownerStatusAlert(%q) notifies = %v, want %v", status, ok, want)
			}
			if !ok {
				return
			}
			if a.title == "" || a.body == "" {
				t.Errorf("ownerStatusAlert(%q) = %+v, want both a title and a body", status, a)
			}
		})
	}
}

// TestRiderCancelledClassifiesTheFrom is the MYR-585 classification table, kept
// separate from the notifier's audience test because it is the piece that
// decides WHICH SENTENCE goes out, and getting it wrong puts a stand-down
// ("no need to continue") on the phone of an owner who never set off.
func TestRiderCancelledClassifiesTheFrom(t *testing.T) {
	rider, owner := rideCancelledByRiderStamp, cancelledByOwner

	tests := []struct {
		name        string
		status      string
		cancelledBy *string
		previous    string
		want        riderCancelKind
	}{
		{name: "requested → the withdrawn request", status: statusCancelled, cancelledBy: &rider, previous: statusRequested, want: riderCancelRequest},
		{name: "accepted → committed", status: statusCancelled, cancelledBy: &rider, previous: statusAccepted, want: riderCancelCommitted},
		{name: "arrived → committed", status: statusCancelled, cancelledBy: &rider, previous: statusArrived, want: riderCancelCommitted},
		{name: "enroute → committed", status: statusCancelled, cancelledBy: &rider, previous: statusEnroute, want: riderCancelCommitted},
		{name: "no previous status is unclassifiable", status: statusCancelled, cancelledBy: &rider, previous: "", want: riderCancelNone},
		{name: "a terminal previous status is unclassifiable", status: statusCancelled, cancelledBy: &rider, previous: statusCompleted, want: riderCancelNone},
		{name: "an OWNER stamp is not this fork", status: statusCancelled, cancelledBy: &owner, previous: statusAccepted, want: riderCancelNone},
		{name: "an unstamped legacy cancel", status: statusCancelled, cancelledBy: nil, previous: statusAccepted, want: riderCancelNone},
		{name: "a non-cancel transition", status: statusAccepted, cancelledBy: &rider, previous: statusRequested, want: riderCancelNone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := riderCancelled(tt.status, tt.cancelledBy, tt.previous); got != tt.want {
				t.Errorf("riderCancelled(%q, %v, %q) = %v, want %v", tt.status, tt.cancelledBy, tt.previous, got, tt.want)
			}
		})
	}
}

// TestOwnerStatusAlertCancelForkOutranksTheStatus pins that both cancel kinds
// produce copy regardless of the status string they arrive with — the fork is
// keyed on the CLASSIFICATION, not on a second reading of the transition, so
// the two cannot disagree.
//
// This OVERLAPS TestNotifierRiderCancelWakesTheOwner on the title/body
// assertions, deliberately, and the two should not be collapsed: that one
// drives the whole notifier and answers "did the right PHONE ring?", this one
// calls the copy function directly and answers "is the right SENTENCE
// produced?". Only this one can reach a kind the notifier would never build,
// which is what keeps the enum honest if the classifier ever changes.
func TestOwnerStatusAlertCancelForkOutranksTheStatus(t *testing.T) {
	name := "Sam"

	tests := []struct {
		name      string
		kind      riderCancelKind
		requester *string
		wantTitle string
	}{
		{name: "withdrawn request, named", kind: riderCancelRequest, requester: &name, wantTitle: "Sam cancelled their ride request"},
		{name: "withdrawn request, nameless", kind: riderCancelRequest, requester: nil, wantTitle: "Your rider cancelled their ride request"},
		{name: "committed ride, named", kind: riderCancelCommitted, requester: &name, wantTitle: "Sam cancelled the ride"},
		{name: "committed ride, nameless", kind: riderCancelCommitted, requester: nil, wantTitle: "Your rider cancelled the ride"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, ok := ownerStatusAlert(statusCancelled, tt.requester, tt.kind)
			if !ok {
				t.Fatal("produced no alert")
			}
			if a.title != tt.wantTitle {
				t.Errorf("title = %q, want %q", a.title, tt.wantTitle)
			}
			if a.body == "" {
				t.Error("body is empty")
			}
			// Payload policy: nothing but a first name may travel.
			if strings.Contains(a.title+" "+a.body, "@") {
				t.Errorf("copy %q %q leaked an address-shaped value", a.title, a.body)
			}
		})
	}
}

// TestOwnerStatusAlertNamesOnlyTheFirstName pins the payload policy on the new
// send site: the owner's banner may carry the rider's FIRST name and nothing
// else. A nil or blank name must degrade to the generic sentence rather than
// producing a ragged " started the ride".
func TestOwnerStatusAlertNamesOnlyTheFirstName(t *testing.T) {
	tests := []struct {
		name      string
		requester *string
		wantTitle string
	}{
		{name: "nil name", requester: nil, wantTitle: "Your rider started the ride"},
		{name: "blank name", requester: strptr("   "), wantTitle: "Your rider started the ride"},
		{name: "first name only", requester: strptr("Aarthi"), wantTitle: "Aarthi started the ride"},
		{name: "full name is trimmed to first", requester: strptr("Aarthi Sivasankar"), wantTitle: "Aarthi started the ride"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, ok := ownerStatusAlert(statusEnroute, tt.requester, riderCancelNone)
			if !ok {
				t.Fatal("ownerStatusAlert(enroute) produced no alert")
			}
			if a.title != tt.wantTitle {
				t.Errorf("title = %q, want %q", a.title, tt.wantTitle)
			}
			if a.body != bodyFollowAlong {
				t.Errorf("body = %q, want %q", a.body, bodyFollowAlong)
			}
		})
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
	instant, ok := statusAlert(statusDeclined, "Blue Whale", false, false)
	if !ok {
		t.Fatal("instant declined produced no alert")
	}
	scheduled, ok := statusAlert(statusDeclined, "Blue Whale", true, false)
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
		a, _ := statusAlert(status, "Blue Whale", false, false)
		b, _ := statusAlert(status, "Blue Whale", true, false)
		if a != b {
			t.Errorf("statusAlert(%q) must not fork on scheduling: %+v vs %+v", status, a, b)
		}
	}
}
