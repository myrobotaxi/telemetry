package push

import (
	"context"
	"time"
	"unicode/utf8"
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
// Apple caps the whole payload at 4KB and throttles high-frequency Activity
// pushes by budget, and every field here has to survive being wrong for up to
// three minutes (see StaleAfter) — so the bar for inclusion is "the rider would
// misread the screen without it", not "we have the value handy". That bar is
// also why the r16 redesign added ONE field: the "Meet at {pickup}" line it
// introduced needs no push, because the pickup cannot change for the life of a
// ride and the app already holds it (rest-api.md §7.21.3).
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
	// also what lets the tick cadence (24–36s since MYR-573) look continuous.
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

	// Progress is how far along the CURRENT leg the car is, 0..1, omitted
	// entirely when we cannot say (MYR-398).
	//
	// Which leg is not carried: `Status` already says it (`accepted` is the car
	// coming to the rider, `enroute` is the car taking them onward), and a
	// second copy of one fact is a second thing that can disagree with itself.
	//
	// P0 and worth saying why, since its neighbour `Destination` is not: this is
	// a unitless fraction of an unnamed journey. It carries no coordinate, no
	// place name, and no distance — "62% of the way" locates the car only for
	// somebody who already knows both ends of the trip, which on this surface is
	// the rider whose trip it is. It is therefore the one field here that could
	// ride an owner-side Activity unchanged, if one is ever started.
	//
	// Absent is a first-class value and the common one: a car with no active nav
	// route, a car whose telemetry has gone quiet, an Activity registered before
	// dispatch. The client renders the card with no track. See
	// activity_progress.go for the derivation and its honesty bounds.
	Progress *float64 `json:"progress,omitempty"`

	// AsOf is when the DATA in this content-state was true, in unix seconds
	// (MYR-398, the v3 card). The client renders it as the stale presentation's
	// subline, "Last updated {h:mm A}".
	//
	// IT IS NOT ActivityNotification.Timestamp, and keeping the two apart is the
	// entire reason the field exists. `aps.timestamp` is when the push was SENT
	// and is what ActivityKit uses to discard an out-of-order update, so it must
	// always be `now`. `AsOf` is when the server last LEARNED something, and it
	// is meant to lag.
	//
	// THE CASE IT EXISTS FOR is the one activity_progress.go's ProgressFreshFor
	// and rest-api.md §7.21.3 already confess to. The ticker pushes
	// unconditionally; the stale-date is `timestamp + 3 min` and is therefore
	// re-armed by the push itself; `eta` is rebuilt from each push's own `now`.
	// So a car that goes quiet mid-leg renders a HELD track beside a
	// perpetually renewed arrival time, ActivityKit never marks the card stale,
	// and nothing on it explains the freeze. This is that missing sentence —
	// which is also exactly why it MUST NOT re-stamp to `now` on a pass that
	// computed nothing new. See contentState for the derivation.
	//
	// Every content-state contentState builds carries one, unlike its two
	// optional neighbours: there is no case in which the server cannot say when
	// it computed a state. It is a POINTER anyway, and that is a deliberate
	// application of `progress`'s own rule rather than symmetry for its own
	// sake — the zero value of an int64 unix timestamp is not a neutral
	// placeholder, it is the claim "this data was true in January 1970", and a
	// card rendering "Last updated 12:00 AM" is worse than one rendering
	// nothing. An absent key is the only safe way for this field to be unset.
	AsOf *int64 `json:"asOf,omitempty"`
}

// MaxContentStateLabel bounds the free-text labels in a content-state, in
// RUNES, matching `maxLength: 128` on `destination` and `vehicleName` in
// schemas/live-activity.schema.json.
//
// The contract declared the bound; nothing enforced it. `destination` is the
// rider's own label for a saved place and `vehicleName` is the owner's nickname
// for the car — both arrive from a request body and neither is bounded on the
// way in, so a 4,000-character "Home" was one POST away from pushing the whole
// payload past Apple's 4KB ceiling. Apple's rejection for that is a flat 400
// with no route back to the cause, and it would take out the ENTIRE Activity
// for that ride — every subsequent ETA tick included — not merely the long
// field.
//
// Enforced here, at the content-state build, rather than at the write: it is
// the one place every send path passes through, and it also covers the labels
// already in the database from before the bound existed.
const MaxContentStateLabel = 128

// truncateLabel shortens s to at most MaxContentStateLabel runes, marking the
// cut with an ellipsis so the rider can see the label was shortened rather than
// silently misreading a truncated address.
//
// It differs from truncateRunes beside it (which caps a first name flat) only
// in that ellipsis, and the difference is deliberate: a name cut short still
// reads as a name, whereas "1 Infinite Loop, Cuperti" reads as an address the
// rider does not recognise.
//
// Counted in RUNES, not bytes, and that is not pedantry: byte-slicing UTF-8
// splits a multi-byte character, and encoding/json re-encodes the broken tail
// as U+FFFD — so a destination in Japanese or with an emoji would arrive on the
// lock screen ending in a replacement character. The ellipsis is counted within
// the budget, so the result is never longer than the bound the schema promises.
func truncateLabel(s string) string {
	if utf8.RuneCountInString(s) <= MaxContentStateLabel {
		return s
	}
	// -1 leaves room for the ellipsis INSIDE the budget, so the result is never
	// longer than the bound the schema promises.
	return truncateRunes(s, MaxContentStateLabel-1) + "…"
}

// StaleAfter is how far past a send the content-state stays trustworthy.
//
// MYR-194's honesty policy: ActivityKit renders its own "as of X min ago"
// treatment once stale-date passes, so a phone that stopped receiving pushes
// says so instead of presenting a three-hour-old ETA as current. Three minutes
// is several missed ticks at the 24–36s cadence (MYR-573; it was "a little
// over two" at the old 60–90s one, and it deliberately did NOT shrink with the
// cadence — a shorter window would flap the display on a brief APNs hiccup,
// and the honesty it buys is already bought) — long enough that a dropped push
// does not flap the display, short enough that a rider is never confidently
// misinformed.
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
	// LowPriority sends at apns-priority 5 instead of 10. ⚠️ NO PRODUCTION
	// CALLER SETS IT SINCE MYR-573: the ETA ticker rode it per MYR-194
	// decision 3, and field evidence reversed that — priority-5 Activity
	// updates are deferred indefinitely on a locked phone, so the card only
	// ever moved on lifecycle alerts. Kept as the deliberate retreat shape
	// (one line in the ticker re-enables it if Apple ever throttles the
	// immediate budget), and pinned by TestActivityTickUsesConservingPriority
	// so the header mapping cannot rot. Ignored when Alert is set — see
	// priority().
	LowPriority bool
	// Alert, when set, adds an `aps.alert` dictionary that makes iOS expand the
	// Dynamic Island for ~3s (MYR-398). Nil on all but the six phase changes;
	// see activity_alert.go for why "nil" is the overwhelmingly common case.
	Alert *ActivityAlert
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
//
// AN ALERTING UPDATE IS ALWAYS IMMEDIATE, whatever the caller asked for — a
// push whose entire purpose is to open the island for three seconds is not a
// refresh that can afford to be dropped or coalesced. (Historical note: until
// MYR-573 the ETA ticker set LowPriority, so this promotion was load-bearing on
// every Arriving alert; today no production caller sets the flag and the guard
// is defence for the retreat path.)
func (n ActivityNotification) priority() string {
	if n.LowPriority && n.Alert == nil {
		return priorityConserving
	}
	return priorityImmediate
}
