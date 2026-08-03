// Verification for the MYR-433 plaintext purge: decide, per row, whether
// the ciphertext sibling is a good enough copy to justify destroying the
// plaintext.
//
// This file is the whole safety argument of the package. See the package
// comment for why "decrypts" — not "decrypts AND matches" — is the
// correct gate.

package plaintextpurge

import (
	"encoding/json"
	"log/slog"
	"reflect"
	"strconv"

	"github.com/myrobotaxi/telemetry/internal/store/routeblob"
)

// verdict is the per-target verification outcome.
type verdict int

const (
	// verdictScrub means every member with data has a ciphertext that
	// decrypts, so the plaintext is redundant or stale and can go.
	verdictScrub verdict = iota
	// verdictUnsealed means at least one member has plaintext but no
	// ciphertext. That plaintext is the only copy — scrubbing would be
	// data loss.
	verdictUnsealed
	// verdictUndecryptable means at least one member's ciphertext exists
	// but will not decrypt under the configured key.
	verdictUndecryptable
)

// verifyTarget decides a whole target at once.
//
// All-or-nothing: if ANY member blocks, the target blocks and no column
// is written. That is what keeps a GPS pair honest — scrubbing a verified
// latitude while its longitude stayed behind would both leave a readable
// coordinate and half-destroy the pair.
//
// The second return reports whether any scrubbable member was STALE
// (decrypted fine but differed). That is purely for the operator's
// counters; it never affects the decision.
func (p *Purger) verifyTarget(id string, scans []memberScan) (verdict, bool) {
	anyStale := false

	for _, s := range scans {
		m := s.member

		// A member that holds nothing needs no ciphertext and cannot
		// block. This is the already-scrubbed half of a pair, or a
		// genuinely empty nullable column.
		if !s.hasData || s.plaintext == nil {
			continue
		}

		if s.ciphertext == nil || *s.ciphertext == "" {
			p.logBlocked(m, id, "no ciphertext sibling; run the matching backfill first")
			return verdictUnsealed, false
		}

		decrypted, err := p.decrypt(m, *s.ciphertext)
		if err != nil {
			p.logBlocked(m, id, "ciphertext did not decrypt: "+err.Error())
			return verdictUndecryptable, false
		}

		if !valuesMatch(m.Kind, *s.plaintext, decrypted) {
			// Stale, not suspect. Nothing writes this plaintext any more,
			// so it is an older snapshot of a field whose current value we
			// just decrypted successfully. Scrub it — that is the entire
			// point of the tool. Deliberately does NOT log the values:
			// this runs over Tesla credentials and GPS coordinates, and a
			// log line carrying them would recreate the exposure in the
			// log aggregator.
			anyStale = true
		}
	}
	return verdictScrub, anyStale
}

// decrypt unseals one ciphertext according to the member's Kind.
//
// The JSON columns went through routeblob (which base64s the JSON bytes
// before sealing); the float and string columns were sealed directly with
// EncryptString. Using the same helper the writer used is what makes the
// result meaningful.
func (p *Purger) decrypt(m Member, ciphertext string) (string, error) {
	if m.Kind == KindJSON {
		raw, err := routeblob.DecryptJSONBytes(ciphertext, p.encryptor)
		if err != nil {
			return "", err //nolint:wrapcheck // caller renders the message
		}
		return string(raw), nil
	}
	v, err := p.encryptor.DecryptString(ciphertext)
	if err != nil {
		return "", err //nolint:wrapcheck // caller renders the message
	}
	return v, nil
}

// valuesMatch reports whether the plaintext and the decrypted ciphertext
// are the same value, using the comparison appropriate to the Kind.
//
// A false result means "stale", not "corrupt" — see verifyTarget.
func valuesMatch(kind Kind, plaintext, decrypted string) bool {
	switch kind {
	case KindString:
		return plaintext == decrypted

	case KindFloat:
		// Compare as parsed floats, not as text. Postgres renders a double
		// precision column its own way and Go's strconv.FormatFloat(-1)
		// renders it another; "37.7" and "37.70" are the same coordinate.
		pf, pErr := strconv.ParseFloat(plaintext, 64)
		df, dErr := strconv.ParseFloat(decrypted, 64)
		if pErr != nil || dErr != nil {
			return false
		}
		return pf == df

	case KindJSON:
		return jsonEquivalent(plaintext, decrypted)
	}
	return false
}

// jsonEquivalent reports whether two JSON documents have the same decoded
// shape.
//
// A byte comparison would report differences for entirely healthy rows:
// the plaintext side is read back out of a jsonb column, which Postgres
// has already normalised (whitespace stripped, object keys reordered,
// numbers canonicalised), while the ciphertext preserves whatever bytes
// were sealed. Decoding both sides answers the question we actually care
// about — is the same route recoverable?
func jsonEquivalent(a, b string) bool {
	var av, bv any
	if err := json.Unmarshal([]byte(a), &av); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(b), &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// logBlocked records a row the purge refused to touch. The row id is safe
// to log; the column value never is.
func (p *Purger) logBlocked(m Member, id, reason string) {
	if p.logger == nil {
		return
	}
	p.logger.Warn("plaintextpurge: row left in place",
		slog.String("column", m.Plaintext),
		slog.String("id", id),
		slog.String("reason", reason),
	)
}
