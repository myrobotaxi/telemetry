// Verification for the MYR-433 plaintext purge: decide, per row, whether
// the ciphertext sibling faithfully reproduces the plaintext about to be
// destroyed.
//
// This file is the whole safety argument of the package. Everything it
// returns other than verdictOK means "leave the data alone".

package plaintextpurge

import (
	"encoding/json"
	"log/slog"
	"reflect"
	"strconv"

	"github.com/myrobotaxi/telemetry/internal/store/routeblob"
)

// verdict is the per-row verification outcome.
type verdict int

const (
	// verdictOK means the ciphertext decrypts and matches the plaintext,
	// so the plaintext is redundant and safe to scrub.
	verdictOK verdict = iota
	// verdictUnsealed means there is no ciphertext. The plaintext is the
	// only copy — scrubbing would be data loss.
	verdictUnsealed
	// verdictMismatch means the ciphertext decrypted to something other
	// than the plaintext. One of the two is wrong and this tool must not
	// be the one to decide which.
	verdictMismatch
	// verdictUndecryptable means the ciphertext exists but will not
	// decrypt under the configured key.
	verdictUndecryptable
)

// verify decrypts the ciphertext sibling and compares it against the
// plaintext value, dispatching on the column's Kind.
func (p *Purger) verify(col Column, id, plaintext string, ciphertext *string) verdict {
	if ciphertext == nil || *ciphertext == "" {
		p.logBlocked(col, id, "no ciphertext sibling; run the matching backfill first")
		return verdictUnsealed
	}

	decrypted, err := p.decrypt(col, *ciphertext)
	if err != nil {
		p.logBlocked(col, id, "ciphertext did not decrypt: "+err.Error())
		return verdictUndecryptable
	}

	if !valuesMatch(col.Kind, plaintext, decrypted) {
		// Deliberately does NOT log the values. This runs over Tesla
		// credentials and GPS coordinates; a mismatch report that printed
		// them would recreate the exposure in the log aggregator.
		p.logBlocked(col, id, "ciphertext decrypted but does not match the plaintext")
		return verdictMismatch
	}
	return verdictOK
}

// decrypt unseals one ciphertext according to the column's Kind.
//
// The JSON columns went through routeblob (which base64s the JSON bytes
// before sealing); the float and string columns were sealed directly with
// EncryptString. Using the same helper the writer used is what makes this
// comparison meaningful.
func (p *Purger) decrypt(col Column, ciphertext string) (string, error) {
	if col.Kind == KindJSON {
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

// valuesMatch compares a plaintext column value against the decrypted
// ciphertext, using the comparison appropriate to the column's Kind.
func valuesMatch(kind Kind, plaintext, decrypted string) bool {
	switch kind {
	case KindString:
		return plaintext == decrypted

	case KindFloat:
		// Compare as parsed floats, not as text. Postgres renders a double
		// precision column its own way and Go's strconv.FormatFloat(-1)
		// renders it another; "37.7" and "37.70" are the same coordinate.
		// Both sides must parse — an unparseable side is not a match.
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
// A byte comparison would produce false mismatches for entirely healthy
// rows: the plaintext side is read back out of a jsonb column, which
// Postgres has already normalised (whitespace stripped, object keys
// reordered, numbers canonicalised), while the ciphertext preserves
// whatever bytes were sealed. Decoding both sides is the only comparison
// that answers the question we actually care about — is the same route
// recoverable from the ciphertext?
//
// Numbers decode to float64 on both sides, so coordinate values compare
// exactly as written.
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

// logBlocked records a row the purge refused to touch. The row id is
// safe to log; the column value never is.
func (p *Purger) logBlocked(col Column, id, reason string) {
	if p.logger == nil {
		return
	}
	p.logger.Warn("plaintextpurge: row left in place",
		slog.String("column", col.Label()),
		slog.String("id", id),
		slog.String("reason", reason),
	)
}
