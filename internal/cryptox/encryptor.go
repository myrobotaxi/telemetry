package cryptox

import (
	"encoding/base64"
	"fmt"
	"log/slog"
)

// Encryptor is the public surface for column-level encryption (NFR-3.23).
// All P1 fields per docs/contracts/data-classification.md §3.3 are
// expected to round-trip through this interface in the eventual rollout.
//
// The interface accepts/returns base64 strings on the wire because every
// P1 column today is a Postgres Text or JSON value — base64 keeps the
// stored shape uniform. Raw byte ciphertext is intentionally NOT exposed
// outside the package.
//
// # Empty-string sentinel
//
// The empty string round-trips through both methods unchanged: EncryptString("")
// returns ("", nil) and DecryptString("") returns ("", nil). This sentinel
// represents an absent value (NULL column, optional field) so the store
// layer can keep the encrypt-on-write/decrypt-on-read code path uniform
// for nullable P1 columns. Callers MUST NOT use the empty string to
// encrypt genuine zero-length payloads — there is no signal distinguishing
// "absent" from "encrypted empty". If a future column requires
// authenticated empty payloads, add an explicit Encrypt(plaintext []byte)
// method that bypasses the sentinel.
type Encryptor interface {
	// EncryptString seals s under the active write key and returns the
	// base64-encoded ciphertext blob (version || nonce || ct || tag).
	// Empty input returns the empty string per the package sentinel
	// contract above.
	EncryptString(s string) (string, error)

	// DecryptString opens a base64 ciphertext produced by EncryptString
	// (or a compatible producer using the same KeySet). Returns
	// ErrCiphertextTooShort, ErrInvalidVersion, or ErrUnknownKeyVersion
	// for malformed input; returns a wrapped GCM auth-failure error for
	// tampered/wrong-key input. Empty input returns the empty string per
	// the package sentinel contract above.
	DecryptString(ciphertext string) (string, error)
}

// keySetEncryptor is the concrete Encryptor backed by a KeySet. The
// version byte at position 0 of every ciphertext routes Decrypt to the
// matching key in keys, supporting in-place key rotation without
// re-encrypting old ciphertexts up front (see docs/contracts/key-rotation.md).
type keySetEncryptor struct {
	ks      *KeySet
	metrics Metrics
}

// Option configures the Encryptor at construction time. Use
// WithMetrics to wire a Prometheus-backed Metrics; defaults to
// NoopMetrics if no option is supplied.
type Option func(*keySetEncryptor)

// WithMetrics injects a Metrics implementation that the Encryptor
// calls on every successful Decrypt. The composition root in
// cmd/telemetry-server uses this to wire the
// `cryptox_decrypt_total{version="N"}` Prometheus counter; library
// consumers without a registry can skip the option to get the
// zero-cost no-op default.
func WithMetrics(m Metrics) Option {
	return func(e *keySetEncryptor) {
		if m != nil {
			e.metrics = m
		}
	}
}

// LogValue redacts the encryptor when a structured logger walks pointer
// fields (e.g., slog.Any("enc", enc)). Defense-in-depth alongside
// KeySet.String — even though the encryptor doesn't directly expose key
// material, slog.Any can recurse into struct fields on some handlers.
func (e *keySetEncryptor) LogValue() slog.Value {
	return slog.StringValue("<Encryptor:redacted>")
}

// NewEncryptor wraps a KeySet in an Encryptor. Returns an error if ks
// is nil or has no write key (defensive — LoadKeySetFromEnv already
// enforces this, but in-process construction by tests bypasses that
// path). Pass WithMetrics to wire decrypt observability; defaults to
// NoopMetrics so library consumers without a Prometheus registry pay
// no allocation cost.
func NewEncryptor(ks *KeySet, opts ...Option) (Encryptor, error) {
	if ks == nil {
		return nil, fmt.Errorf("cryptox.NewEncryptor: nil KeySet")
	}
	if _, ok := ks.keyForVersion(ks.writeVersion); !ok {
		return nil, fmt.Errorf("cryptox.NewEncryptor: write version %d has no key in KeySet", ks.writeVersion)
	}
	e := &keySetEncryptor{ks: ks, metrics: NoopMetrics{}}
	for _, opt := range opts {
		opt(e)
	}
	return e, nil
}

// EncryptString seals s, prepends the active write version byte, and
// returns base64-encoded output.
func (e *keySetEncryptor) EncryptString(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	writeKey, ok := e.ks.keyForVersion(e.ks.writeVersion)
	if !ok {
		// Defensive — NewEncryptor already validated this.
		return "", fmt.Errorf("cryptox.EncryptString: write key missing for version %d", e.ks.writeVersion)
	}
	body, err := encryptGCM(writeKey, []byte(s))
	if err != nil {
		return "", err
	}
	blob := make([]byte, 0, 1+len(body))
	blob = append(blob, e.ks.writeVersion)
	blob = append(blob, body...)
	return base64.StdEncoding.EncodeToString(blob), nil
}

// DecryptString reverses EncryptString. Routes by version byte; rejects
// ciphertexts whose version byte has no key in the KeySet.
func (e *keySetEncryptor) DecryptString(ciphertext string) (string, error) {
	if ciphertext == "" {
		return "", nil
	}
	blob, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("cryptox.DecryptString: base64 decode: %w", err)
	}
	if len(blob) < MinCiphertextLen {
		return "", ErrCiphertextTooShort
	}
	version := blob[0]
	if version == 0x00 {
		return "", ErrInvalidVersion
	}
	key, ok := e.ks.keyForVersion(version)
	if !ok {
		return "", fmt.Errorf("%w: version=%d", ErrUnknownKeyVersion, version)
	}
	plaintext, err := decryptGCM(key, blob[1:])
	if err != nil {
		return "", err
	}
	// Increment AFTER success: failed decrypts (auth-tag mismatch,
	// truncated input, unknown version, base64 errors) MUST NOT
	// count, otherwise a tampering attack inflates the counter and
	// hides the v1-decay signal during rotation
	// (key-rotation.md §"Procedure" step 6).
	e.metrics.IncDecrypt(version)
	return string(plaintext), nil
}
