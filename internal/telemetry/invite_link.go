package telemetry

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Signed invite links (MYR-368, rest-api.md §7.5.6).
//
// An owner shares a URL, not six dictated characters. The URL carries the code
// in its path and an Ed25519 signature in `?k=`, and the web join shell
// verifies that signature against a PUBLIC key compiled into the shell — no
// database, no API call, no round trip. That is the whole point of the design:
// a forged or expired link is bounced by static JavaScript, so the join page
// can never be turned into an oracle that answers "is RBO247 a real code?" for
// an attacker walking the 36^6 space.
//
// What the signature does NOT prove is that the invite is still live. A link
// can verify perfectly and still be cancelled, resent, or already redeemed —
// only POST /api/invites/redeem decides that, and it is unchanged. The
// signature buys exactly one thing: the shell can discard obvious garbage
// without asking the server.
//
// The PRIVATE key exists only here, seeded from INVITE_LINK_SIGNING_KEY. See
// cmd/telemetry-server/invite_link_signer.go for the startup resolution (a
// missing seed is a hard startup failure outside --dev) and
// `ops invite-link public-key` for extracting the public half to paste into the
// web repo.

// InviteLinkBaseURL is the join shell's base. The code is appended verbatim as
// the final path segment.
const InviteLinkBaseURL = "https://myrobotaxi.app/join/"

// inviteLinkKeyID is the one-character key id that leads the `k` parameter.
//
// It is a constant today and it earns its place anyway: it is the seam that
// makes ROTATION possible without invalidating every link in flight. To rotate,
// the shell learns key '2' alongside '1', the server starts signing with '2',
// and links signed under '1' keep verifying until the last of them expires
// seven days later. Without the id the shell could only hold one key at a time
// and a rotation would break every unredeemed invite the moment it happened.
//
// One character, not a version string, because it rides in a URL an owner may
// read aloud.
const inviteLinkKeyID = "1"

// inviteLinkPayloadPrefix leads the signed payload. It DOMAIN-SEPARATES this
// signature from any other use of the same key: a signature minted here can
// never be replayed as a signature over something else, because every message
// this key ever signs starts with "join:".
const inviteLinkPayloadPrefix = "join:"

// maxInviteLinkNameLen caps each display name in the link. Twenty ASCII
// letters is longer than any first name that renders on a phone-width join
// screen, and the cap exists mostly so the query string cannot be used as a
// channel for arbitrary owner-typed text.
const maxInviteLinkNameLen = 20

// ErrInviteLinkSeedInvalid reports a malformed INVITE_LINK_SIGNING_KEY. It
// never carries the offending value — the seed is P0 secret material, and an
// error string is exactly the sort of place a secret leaks into a log.
var ErrInviteLinkSeedInvalid = errors.New("invite link signing seed is invalid")

// InviteLinkSigner mints signed join URLs. Immutable after construction and
// safe for concurrent use: ed25519.Sign only reads the key.
//
// A nil *InviteLinkSigner is a valid, usable value meaning "no signing key
// configured" — every method degrades to the empty string. That is deliberate:
// the keyless path must drop `shareUrl` from the wire (a consumer then falls
// back to the bare `code`) rather than emit an unsigned link the shell would
// bounce, which would look broken to whoever received it.
type InviteLinkSigner struct {
	key ed25519.PrivateKey
	// pubB64 is precomputed so the startup log line and the ops subcommand
	// do not each re-derive it.
	pubB64 string
}

// NewInviteLinkSigner builds a signer from a 32-byte Ed25519 seed.
func NewInviteLinkSigner(seed []byte) (*InviteLinkSigner, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%w: seed must be %d bytes, got %d",
			ErrInviteLinkSeedInvalid, ed25519.SeedSize, len(seed))
	}
	key := ed25519.NewKeyFromSeed(seed)
	pub, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("%w: unexpected public key type", ErrInviteLinkSeedInvalid)
	}
	return &InviteLinkSigner{
		key:    key,
		pubB64: base64.StdEncoding.EncodeToString(pub),
	}, nil
}

// NewInviteLinkSignerFromSeedBase64 builds a signer from the base64 seed as it
// is stored in the INVITE_LINK_SIGNING_KEY secret. Surrounding whitespace is
// trimmed, because a secret pasted through a shell frequently carries a
// trailing newline and a key that silently fails to load is worse than one that
// refuses to.
func NewInviteLinkSignerFromSeedBase64(s string) (*InviteLinkSigner, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: empty", ErrInviteLinkSeedInvalid)
	}
	seed, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		// The decode error is deliberately NOT wrapped: encoding/base64
		// puts the offending input in its message, and the input here is
		// the signing seed.
		return nil, fmt.Errorf("%w: not valid base64", ErrInviteLinkSeedInvalid)
	}
	return NewInviteLinkSigner(seed)
}

// PublicKeyBase64 returns the standard-base64 Ed25519 public key — the value
// pasted into the web repo's join-shell constant. Public by definition and safe
// to log. Empty when there is no key.
func (s *InviteLinkSigner) PublicKeyBase64() string {
	if s == nil {
		return ""
	}
	return s.pubB64
}

// inviteLinkCtx is everything minting a signed `shareUrl` needs beyond the
// invite row itself. It travels as one value rather than two more positional
// parameters so the projection helpers keep a readable signature, and so a
// caller cannot accidentally pass a recipient label where an owner name goes.
//
// ownerName is the OWNER's resolved display name — the `from` half. The
// recipient half comes off the row's own Label, because that is the only place
// the invited person is named. Both are raw here; sanitization belongs to
// ShareURL.
type inviteLinkCtx struct {
	signer    *InviteLinkSigner
	ownerName string
}

// ShareURL returns the complete signed join URL for a pending invite, or "" if
// there is nothing honest to return: no key, no code, or no expiry.
//
//	https://myrobotaxi.app/join/{code}?k={kid}.{exp}.{sig}&from={from}&to={to}
//
// expiresAt MUST be the invite row's ACTUAL expires_at, not a TTL re-derived
// from the current time. The database computes the expiry (`NOW() + INTERVAL
// '7 days'`, evaluated on the database's clock) and the redeem predicate reads
// that same column, so signing anything else would put a different instant in
// the link than the one redemption enforces.
//
// ownerName and recipientLabel are the RAW values (the owner's resolved display
// name and the owner-typed invite label). Sanitization happens HERE and nowhere
// else, so the bytes that go in the query string and the bytes that go into the
// signature can never drift apart — the single most likely way to ship a link
// that fails its own verifier. Either name may sanitize away to nothing, in
// which case its parameter is omitted from the URL and signed as the empty
// string.
func (s *InviteLinkSigner) ShareURL(code string, expiresAt time.Time, ownerName, recipientLabel string) string {
	if s == nil || code == "" || expiresAt.IsZero() {
		return ""
	}
	exp := strconv.FormatInt(expiresAt.Unix(), 10)
	from := sanitizeInviteLinkName(ownerName)
	to := sanitizeInviteLinkName(recipientLabel)

	sig := ed25519.Sign(s.key, []byte(inviteLinkPayload(code, exp, from, to)))
	// base64url without padding: every character is URL-safe, so `k`
	// survives a query string byte-for-byte and the shell verifies over the
	// same string the server signed.
	k := inviteLinkKeyID + "." + exp + "." + base64.RawURLEncoding.EncodeToString(sig)

	// Built by hand rather than with url.Values.Encode() because that sorts
	// keys alphabetically (from, k, to) and the documented parameter order
	// is k, from, to. Order is cosmetic to a correct parser and load-bearing
	// to a human reading the link, and the contract names one order.
	// url.QueryEscape is a no-op on a sanitized name by construction; it is
	// applied anyway so a future relaxation of the alphabet cannot silently
	// emit a malformed query.
	var b strings.Builder
	b.WriteString(InviteLinkBaseURL)
	b.WriteString(code)
	b.WriteString("?k=")
	b.WriteString(k)
	if from != "" {
		b.WriteString("&from=")
		b.WriteString(url.QueryEscape(from))
	}
	if to != "" {
		b.WriteString("&to=")
		b.WriteString(url.QueryEscape(to))
	}
	return b.String()
}

// inviteLinkPayload builds the canonical signed payload:
//
//	join:{code}:{expUnix}:{from}:{to}
//
// Every variable the link carries is covered by the one signature, so none of
// them can be swapped independently — including the two names, which is the
// point of putting them in the payload at all: without it, an attacker holding
// a REAL link could rewrite `from` to any name they liked and the shell would
// render it. An omitted name is signed as the EMPTY STRING, so the payload has
// five fields and four colons no matter what, and "no name" and "a name that
// sanitized to nothing" are the same thing.
//
// The separator is ':'. Nothing that reaches this function can contain one: the
// code alphabet is [A-Z0-9], the expiry is decimal digits, and the two names
// are ASCII letters only. The parse is therefore unambiguous by construction
// rather than by escaping.
func inviteLinkPayload(code, expUnix, from, to string) string {
	return inviteLinkPayloadPrefix + code + ":" + expUnix + ":" + from + ":" + to
}

// sanitizeInviteLinkName reduces a display name to what may appear in a join
// link: the FIRST whitespace-separated token, stripped to ASCII letters, capped
// at maxInviteLinkNameLen. Returns "" when nothing survives, which the caller
// turns into an omitted parameter and an empty payload field.
//
// The three steps are each deliberate:
//
//   - FIRST TOKEN ONLY — the P1 first-names-only policy that governs the push
//     payloads and RideRequest.requesterName. A join link that arrives by text
//     message must not carry somebody's surname.
//   - ASCII LETTERS ONLY — the label is free-form owner-typed text and this
//     value lands in a URL that a shell will render. Restricting the alphabet
//     to letters removes, in one rule, every injection question (no markup, no
//     separators, no percent-encoding, no bidi or zero-width tricks) that a
//     more permissive filter would have to answer case by case. The cost is
//     honest and visible: accented and non-Latin names lose characters or
//     vanish entirely, and vanishing is handled — the parameter is omitted and
//     the shell falls back to its generic copy rather than rendering mojibake.
//   - CAP — a bound on a value that originates as owner-typed input.
func sanitizeInviteLinkName(raw string) string {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return ""
	}
	var b strings.Builder
	for _, r := range fields[0] {
		if b.Len() == maxInviteLinkNameLen {
			break
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
	}
	return b.String()
}
