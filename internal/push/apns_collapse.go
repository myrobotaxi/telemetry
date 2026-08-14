package push

// The `apns-collapse-id` header (MYR-554).
//
// THE BUG IT FIXES IS A DOUBLE-DELIVERY THE SERVER CANNOT SEE. deliver() retries
// one attempt on a network error or a 5xx, and "network error" includes every
// failure that happens AFTER the request left this process: a connection reset
// while the response was being read, a timeout that fired between Apple
// accepting the POST and the ACK arriving, an h2 stream torn down mid-frame.
// In all of those Apple has the notification and the retry sends it AGAIN. Prod
// 2026-08-13 is the proof: two identical owner "time to head out" banners on one
// device, from one device row, behind a claim-guarded publish that ran once —
// so neither the event, nor the fan-out, nor the registry duplicated anything.
// Only the transport did.
//
// THE FIX IS APPLE'S OWN DE-DUPLICATION. Per the APNs provider API (Sending
// notification requests to APNs → "apns-collapse-id"), two notifications
// carrying the SAME collapse id are shown to the user as ONE — the later
// replaces the earlier — and the header's value "must not exceed 64 bytes".
// Giving the two attempts of one send the same id therefore makes a redelivery
// idempotent AT APPLE, which is the only place that can see both copies.
//
// THE ID IS THE NOTIFICATION'S INTENT, not its attempt: `{rideID}:{eventTopic}`.
// It is computed once in Send and carried on the message, so both trips through
// the retry loop present the same value, and it is derived from nothing
// per-attempt (no timestamp, no counter) — an id that varied per attempt would
// be a header Apple honours and a bug it cannot help with.
//
// WHAT ELSE COLLAPSES, stated plainly because it is a behaviour change and not
// only a bug fix. Two alerts for the SAME ride on the SAME topic now merge in
// Notification Center: an `accepted` banner followed later by an `arrived`
// banner (both `ride.status.changed`) leaves the newer one in the list. That is
// the intended reading of a ride — the freshest state of one journey is the one
// worth keeping, and a stack of superseded banners for a ride already under way
// is the noise the Live Activity exists to replace. DELIVERY AND ALERTING ARE
// UNAFFECTED: collapsing merges the entry, it does not suppress the push, so
// every banner still buzzes the phone when it lands.
//
// NOT ON ACTIVITYKIT PUSHES. An Activity update already addresses exactly one
// card through its own update token, so it has nothing to collapse against.
// buildActivityMessage therefore leaves the field empty and the header is
// omitted — see newRequest.

// maxCollapseIDBytes is Apple's documented cap on the `apns-collapse-id` header
// value. A longer value is rejected (400 BadCollapseId), so the id is truncated
// rather than sent whole.
const maxCollapseIDBytes = 64

// collapseID renders the de-duplication key for one alert, or "" when there is
// nothing safe to key on.
//
// AN EMPTY RIDE ID YIELDS NO HEADER, and that is a correctness rule rather than
// tidiness. Every alert this package sends today names a ride, but a future
// fan-out that does not would otherwise collapse on the topic ALONE — merging
// unrelated notifications for unrelated subjects into one line on somebody's
// lock screen. Absent is the only safe answer, and it restores exactly the
// pre-MYR-554 behaviour for such a push: at-least-once, never over-merged.
//
// TRUNCATION IS BY BYTES, on a rune boundary. Both inputs are ASCII today (a
// cuid and a dotted topic constant, ~45 bytes together), so the cap is not
// reached in practice; the guard is here because the header's failure mode is a
// 400 that would silence the notification entirely. Cutting the tail can in
// principle make two long ids equal, and that is the right direction to fail:
// an over-collapse merges two Notification Center entries, where an
// over-length header loses the push.
func collapseID(rideID, eventTopic string) string {
	if rideID == "" {
		return ""
	}
	id := rideID + ":" + eventTopic
	if len(id) <= maxCollapseIDBytes {
		return id
	}
	// Back off to the last whole rune that fits, so the header never carries a
	// half-encoded character.
	cut := maxCollapseIDBytes
	for cut > 0 && !utf8RuneStart(id[cut]) {
		cut--
	}
	return id[:cut]
}

// utf8RuneStart reports whether b begins a UTF-8 rune (i.e. is not a
// continuation byte). Local rather than a unicode/utf8 call so the truncation
// reads as the one-line rule it is.
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
