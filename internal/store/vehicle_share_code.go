package store

import (
	"crypto/rand"
	"fmt"
)

// shareCodeAlphabet is the 36-symbol code alphabet fixed by the contract:
// uppercase A-Z and 0-9, matching ShareInvite.code's ^[A-Z0-9]{6}$ pattern and
// the iOS entry field, which upper-cases input and strips everything else.
//
// Deliberately NOT de-confused (no dropped O/0 or I/1): the pattern in the
// published schema is the full alphabet, and narrowing it here would mint
// codes a conforming client validates fine but shrink the space by a third.
const shareCodeAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// shareCodeLength is the fixed code length (36^6 ≈ 2.2 billion). Small enough
// to read aloud, which is why the redeem endpoint is rate-limited rather than
// relying on the space alone.
const shareCodeLength = 6

// shareCodeRejectionCeiling is the largest multiple of len(shareCodeAlphabet)
// that fits in a byte (36 * 7 = 252). Bytes at or above it are discarded
// instead of folded, because a plain `b % 36` would make the first four
// symbols of the alphabet ~14% more likely than the rest — a bias an attacker
// enumerating a 36^6 space would happily take.
const shareCodeRejectionCeiling = 252

// newShareCode mints a cryptographically random 6-character invite code.
//
// SERVER-MINTED, NEVER CLIENT-SUPPLIED. The returned value is a live bearer
// credential for vehicle access (P1): callers must not log it, must not put it
// in an error message, and must not return it on any non-owner surface.
//
// crypto/rand is the only acceptable source here — a math/rand code is
// predictable from a couple of observed samples, which would make the whole
// sharing surface guessable regardless of the rate limit.
func newShareCode() (string, error) {
	out := make([]byte, 0, shareCodeLength)
	// Read in modest batches and refill on rejection. The expected number of
	// rejected bytes is ~1.6% of draws, so one batch almost always suffices.
	buf := make([]byte, shareCodeLength*2)
	for len(out) < shareCodeLength {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("store.newShareCode: read entropy: %w", err)
		}
		for _, b := range buf {
			if b >= shareCodeRejectionCeiling {
				continue // biased tail — discard, do not fold
			}
			out = append(out, shareCodeAlphabet[int(b)%len(shareCodeAlphabet)])
			if len(out) == shareCodeLength {
				break
			}
		}
	}
	return string(out), nil
}

// ValidShareCodeFormat reports whether s is exactly the wire shape a client
// must send: 6 characters, uppercase A-Z or 0-9. A malformed code is a 400 and
// never reaches the database — matching it would be a wasted lookup and
// answering 404 for it would blur the contract's distinction between "you sent
// nonsense" and "that code does not exist".
func ValidShareCodeFormat(s string) bool {
	if len(s) != shareCodeLength {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			continue
		}
		return false
	}
	return true
}
