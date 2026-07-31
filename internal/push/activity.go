package push

import (
	"context"
	"time"
)

// ActivityKit remote updates (MYR-172, decisions in MYR-194).
//
// A Live Activity is a widget the rider's phone renders on the lock screen and
// in the Dynamic Island for the length of one ride. The app starts it locally
// when the ride is accepted and hands us a per-Activity push token; from then
// on the SERVER owns what it says, by pushing a new `content-state` to that
// token over APNs.
//
// This is a different push channel from the alerts in notifier.go, not a
// variation on one. It goes to a different topic, carries no alert copy, is
// addressed by a token that rotates mid-ride, and — most importantly — is a
// STATE REPLACEMENT rather than a message: each push overwrites what the Live
// Activity displays, so a lost push costs freshness rather than information.
// That is what makes the staleness policy below safe.

// ActivityEvent is the ActivityKit `aps.event` verb.
type ActivityEvent string

const (
	// ActivityEventUpdate replaces the Activity's content-state in place.
	ActivityEventUpdate ActivityEvent = "update"
	// ActivityEventEnd delivers the final content-state and moves the Activity
	// to its dismissed-pending state; the dismissal-date decides when it leaves
	// the lock screen.
	ActivityEventEnd ActivityEvent = "end"
)

// ActivityContentStateVersion is the `v` discriminator carried in every
// content-state.
//
// The Swift ContentState is a Codable struct compiled into an app the user may
// not update for months, so the wire shape is frozen the moment a build ships.
// Versioning it explicitly means the day a field's MEANING changes (not merely
// its presence — new optional fields are already tolerated by Codable) the
// server can keep sending v1 to installed clients while v2 goes to new ones,
// instead of the alternative, which is a lock screen full of wrong numbers on
// every phone that has not updated.
const ActivityContentStateVersion = 1

// ActivityContentState is what the rider's Live Activity renders.
//
// Deliberately four fields. Apple caps the whole payload at 4KB and throttles
// high-frequency Activity pushes by budget, and every field here has to survive
// being wrong for up to three minutes (see StaleAfter) — so the bar for
// inclusion is "the rider would misread the screen without it", not "we have
// the value handy".
type ActivityContentState struct {
	// Version is the content-state schema version. Always
	// ActivityContentStateVersion on send.
	Version int `json:"v"`

	// Status is the ride's lifecycle status, verbatim from the ride record
	// (`accepted`, `arrived`, `enroute`, `completed`, `declined`, `cancelled`).
	// The client maps it to copy; the server never sends prose.
	Status string `json:"status"`

	// ETA is the car's arrival time as an ABSOLUTE unix timestamp in seconds,
	// omitted entirely when unknown.
	//
	// Absolute, not a duration, and that is the whole trick: a duration decays
	// silently on a screen the server cannot repaint ("4 min" stays "4 min" for
	// an hour), whereas an instant stays true no matter how late it is read —
	// the phone subtracts `now` itself and counts down without our help. It is
	// also what lets the ~60–90s cadence look continuous.
	//
	// Absent means we genuinely do not know: the car reports no navigation
	// route. There is no server-side route solver in this service, so the
	// alternative to omitting the key would be inventing a number, which
	// MYR-194 rules out in as many words.
	ETA *int64 `json:"eta,omitempty"`

	// VehicleName is the owner-chosen nickname, "" when the car has none (the
	// client renders its own fallback, as the alert copy does).
	VehicleName string `json:"vehicleName"`

	// Destination is the dropoff's short label — the name the RIDER chose for
	// the place when booking, e.g. "Home".
	//
	// P1. See data-classification.md §1.18: this is the one field on this
	// surface that the alert-copy policy in copy.go would not permit, and it is
	// carried here deliberately and narrowly — a Live Activity is the rider's
	// own ride on the rider's own device, and a ride card that cannot say where
	// the car is taking you is not the feature. It is never sent to the owner's
	// Activity, and never appears in an alert body.
	Destination string `json:"destination"`
}

// StaleAfter is how far past a send the content-state stays trustworthy.
//
// MYR-194's honesty policy: ActivityKit renders its own "as of X min ago"
// treatment once stale-date passes, so a phone that stopped receiving pushes
// says so instead of presenting a three-hour-old ETA as current. Three minutes
// is a little over two missed ticks at the 60–90s cadence — long enough that
// one dropped push does not flap the display, short enough that a rider is
// never confidently misinformed.
const StaleAfter = 3 * time.Minute

// ActivityNotification is one addressed Live Activity update.
type ActivityNotification struct {
	// ActivityToken is the ActivityKit update token. P1 — never log in full.
	ActivityToken string
	// Sandbox selects the APNs sandbox host (development builds).
	Sandbox bool
	// Event is `update` or `end`.
	Event ActivityEvent
	// ContentState is the state the Activity will display.
	ContentState ActivityContentState
	// Timestamp is when this state was true. ActivityKit uses it to discard an
	// update that arrives out of order, which the network makes routine.
	Timestamp time.Time
	// DismissalDate, set only on an end event, is when iOS removes the Activity
	// from the lock screen. Nil on an end event means "dismiss immediately".
	DismissalDate *time.Time
	// LowPriority sends at apns-priority 5 instead of 10. Set for ETA ticks;
	// left false for ride-lifecycle transitions, per MYR-194.
	LowPriority bool
}

// ActivitySender delivers ActivityKit remote updates.
//
// Defined at the consumer site, like Sender beside it, so the Live Activity
// notifier can be tested against a spy without an APNs key.
type ActivitySender interface {
	SendActivity(ctx context.Context, n ActivityNotification) error
}

// StaleDate is when this update stops being trustworthy.
func (n ActivityNotification) StaleDate() time.Time {
	return n.Timestamp.Add(StaleAfter)
}

// priority renders the apns-priority header for this update.
func (n ActivityNotification) priority() string {
	if n.LowPriority {
		return priorityConserving
	}
	return priorityImmediate
}
