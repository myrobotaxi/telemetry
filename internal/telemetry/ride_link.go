package telemetry

import (
	"crypto/ed25519"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

// Signed GROUP-RIDE join links (MYR-540, contracts RideRequest.shareUrl).
//
// This is the MYR-368 signed-link idiom applied to a RIDE instead of a vehicle
// invite, and it is deliberately the SAME machinery: the same Ed25519 private
// key, the same one-character key id, the same `k={kid}.{exp}.{sig}` triple, the
// same base64url-unpadded signature — so the web landing shell verifies both
// kinds against one compiled-in public key and a rotation covers both at once.
// Everything that is not shared lives in this file.
//
// WHAT DIFFERS, AND WHY EACH DIFFERENCE EXISTS:
//
//   - THE PAYLOAD PREFIX IS `ride:`, NOT `join:`. That is DOMAIN SEPARATION and
//     it is the whole reason one key is safe for two link kinds: no signature
//     minted over a ride code can ever verify as a signature over an invite
//     code, because the two messages cannot collide in their first five bytes.
//     A shared key with a shared payload grammar would mean a valid invite link
//     could be re-punctuated into a valid ride link.
//   - THERE ARE NO NAMES IN THE PAYLOAD. An invite link addresses a specific
//     person and carries `from`/`to` so the shell can say who invited whom; a
//     group link addresses NOBODY IN PARTICULAR — it goes into a group chat —
//     so there is no name to render and, more to the point, no name that should
//     travel. The payload is therefore three fields and two colons, fixed.
//   - THE BASE IS /ride/, so the platform's universal-link matcher hands the
//     joiner's app a path it can tell apart from an invite without parsing.
//
// WHAT DOES NOT DIFFER: the signature proves the link CAME FROM THIS SERVER, and
// nothing more. It does not prove the ride is still joinable — redemption stays
// authoritative, and POST /api/ride-requests/join is what decides. The shell's
// static check exists only so a forged or edited link is bounced before it can
// reach anything that could answer "is RBO246 a real code?".

// RideLinkBaseURL is the join landing shell's base. The code is appended
// verbatim as the final path segment.
const RideLinkBaseURL = "https://myrobotaxi.app/ride/"

// rideLinkPayloadPrefix leads the signed payload and DOMAIN-SEPARATES a ride
// link from the invite link that shares its key. See the file header.
const rideLinkPayloadPrefix = "ride:"

// RideShareURL returns the complete signed join URL for a group ride, or "" if
// there is nothing honest to return: no signing key, no code, or no expiry.
//
//	https://myrobotaxi.app/ride/{code}?k={kid}.{exp}.{sig}
//
// expiresAt MUST be the ride row's ACTUAL join_code_expires_at, not a TTL
// re-derived from the current time — the same rule ShareURL follows and for the
// same reason: the database computes the expiry on its own clock and the redeem
// predicate reads that same column, so signing anything else would put a
// different instant in the link than the one redemption enforces.
//
// A nil receiver is a valid, usable value meaning "no signing key configured",
// and it returns "". That is deliberate rather than defensive: the keyless path
// must DROP `shareUrl` from the wire rather than emit an unsigned link the shell
// would bounce. Unlike an invite there is no `code` field to fall back to — a
// group ride with no link simply cannot be shared until the deploy is fixed,
// which is why a missing key refuses to boot outside --dev.
//
// The returned URL CONTAINS the code and is therefore P1 AND BEARER: never log
// it, never put it in an error envelope, and deliver it only on the party-scoped
// REST surfaces.
func (s *InviteLinkSigner) RideShareURL(code string, expiresAt time.Time) string {
	if s == nil || code == "" || expiresAt.IsZero() {
		return ""
	}
	exp := strconv.FormatInt(expiresAt.Unix(), 10)
	sig := ed25519.Sign(s.key, []byte(rideLinkPayload(code, exp)))

	// Built by hand rather than with url.Values.Encode() for the invite link's
	// reason — there is exactly one parameter here, so the sole effect of the
	// helper would be to introduce an escaping question about a value whose
	// alphabet is [0-9A-Za-z._-] by construction.
	var b strings.Builder
	b.WriteString(RideLinkBaseURL)
	b.WriteString(code)
	b.WriteString("?k=")
	b.WriteString(inviteLinkKeyID)
	b.WriteString(".")
	b.WriteString(exp)
	b.WriteString(".")
	// base64url without padding: every character is URL-safe, so `k` survives a
	// query string byte-for-byte and the shell verifies over the same string
	// the server signed.
	b.WriteString(base64.RawURLEncoding.EncodeToString(sig))
	return b.String()
}

// rideLinkPayload builds the canonical signed payload:
//
//	ride:{code}:{expUnix}
//
// Both variables the link carries are covered by the one signature, so neither
// can be swapped independently — in particular the expiry, which is the only
// thing stopping a link the shell has already accepted from being re-dated.
//
// The separator is ':' and the parse is unambiguous BY CONSTRUCTION rather than
// by escaping: the code alphabet is [A-Z0-9] and the expiry is decimal digits,
// so neither field can contain one.
func rideLinkPayload(code, expUnix string) string {
	return rideLinkPayloadPrefix + code + ":" + expUnix
}

// rideShareURLLinger is how long a group ride keeps EMITTING its shareUrl after
// reaching a terminal status.
//
// THE CODE ITSELF DIES AT TERMINAL, with no grace at all: the redeem predicate
// excludes completed/declined/cancelled rides, so a link redeemed one second
// after the drop-off answers 404 like any other dead code. That is the
// contract's rule and it is the one that matters, because it is the one that
// governs access.
//
// The linger governs only the FIELD, and it exists for a rendering reason. A
// ride's terminal transition and the requester's share sheet are not
// coordinated: an owner can tap "Dropped off" while the requester has the sheet
// open, and a `shareUrl` that vanished between two refetches would blank a
// control mid-interaction, which reads as a fault rather than as an ending. Five
// minutes is deliberately the SAME window MYR-421 holds a completed ride's Live
// Activity open for — the existing convention for "how long a ride's surfaces
// outlive the ride" — so there is one number, not two.
//
// Emitting a link that no longer redeems is safe precisely because the contract
// already says so in as many words: the signature proves the link came from the
// server, NOT that the ride is still joinable, and redemption is authoritative.
const rideShareURLLinger = 5 * time.Minute

// rideShareURL projects RideRequest.shareUrl, applying the contract's presence
// rules. Returns "" — the omitted key — whenever any of them fails.
//
// The rules, in the order they are checked:
//
//  1. GROUP RIDES ONLY. A solo ride has no link and never will.
//  2. ACCEPTED OR LATER. The link is minted AT ACCEPT, so there is nothing
//     before it; a request the owner declines never mints one. This is enforced
//     by there being no code rather than by a status test — the mint is what
//     puts one there — so the code's presence IS the accepted-or-later test.
//  3. STILL REDEEMABLE. Past the code's own expiry, nothing is emitted.
//  4. TERMINAL PLUS THE LINGER. See rideShareURLLinger.
//
// ABSENCE IS NEVER A STATEMENT ABOUT WHETHER THIS IS A GROUP RIDE. `groupRide`
// is that field and the only one, which is why the flag is projected
// unconditionally and this is not.
func rideShareURL(d RideRequestData, signer *InviteLinkSigner, now time.Time) string {
	if !d.GroupRide || d.JoinCode == "" || d.JoinCodeExpiresAt == nil {
		return ""
	}
	if !now.Before(*d.JoinCodeExpiresAt) {
		return ""
	}
	if isTerminalRideStatus(d.Status) && !now.Before(rideTerminalLingerEnd(d)) {
		return ""
	}
	return signer.RideShareURL(d.JoinCode, *d.JoinCodeExpiresAt)
}

// rideTerminalLingerEnd is the instant a terminal ride stops emitting its link.
//
// It is measured from UpdatedAt rather than CompletedAt because only ONE of the
// three terminal statuses stamps a completion instant: a declined or cancelled
// ride has no CompletedAt at all, and reading a zero time there would put the
// linger's end in 1970 — i.e. would silently make the linger zero for two of the
// three endings it exists for. UpdatedAt is stamped by every transition, so it
// is the one field that always names when the ride ended.
func rideTerminalLingerEnd(d RideRequestData) time.Time {
	return d.UpdatedAt.Add(rideShareURLLinger)
}
