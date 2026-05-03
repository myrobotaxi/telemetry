package cryptox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

// Ciphertext format constants. Public so external code can size buffers and
// reason about wire-format requirements without depending on internal
// AES-GCM constants.
const (
	// versionV1 is the first ciphertext version emitted by this package.
	// AES-256-GCM with a random 12-byte nonce.
	versionV1 byte = 0x01

	// nonceLen is the GCM standard nonce length. NIST SP 800-38D §5.2.1.1.
	nonceLen = 12

	// gcmTagLen is the AES-GCM authentication tag length appended to ciphertext.
	gcmTagLen = 16

	// keyLen is the AES-256 key length in bytes.
	keyLen = 32

	// MinCiphertextLen is the minimum size in bytes of a valid raw
	// (pre-base64) ciphertext blob: version + nonce + auth tag. A blob
	// shorter than this cannot have come from this package — reject with
	// ErrCiphertextTooShort before invoking AES.
	MinCiphertextLen = 1 + nonceLen + gcmTagLen
)

// Sentinel errors. Callers route by error identity, never on string match.
var (
	// ErrCiphertextTooShort is returned when a ciphertext blob is shorter
	// than the minimum valid size. Defends against panics on truncated or
	// corrupt rows.
	ErrCiphertextTooShort = errors.New("cryptox: ciphertext shorter than minimum")

	// ErrUnknownKeyVersion is returned when Decrypt sees a version byte for
	// which no key is registered in the KeySet. After a key retirement,
	// existing ciphertexts encrypted under the retired key surface this
	// error; the operator must restore the retired key (read-only) or
	// re-encrypt the affected rows before retiring.
	ErrUnknownKeyVersion = errors.New("cryptox: unknown key version")

	// ErrInvalidVersion is returned for the reserved 0x00 byte. 0x00 is
	// reserved as "INVALID" so a zero-initialized buffer at position 0
	// cannot be silently accepted.
	ErrInvalidVersion = errors.New("cryptox: invalid ciphertext version 0x00")

	// ErrInvalidKeyLength is returned when LoadKeySetFromEnv decodes a key
	// that is not exactly 32 bytes (256 bits) after base64 decoding.
	ErrInvalidKeyLength = errors.New("cryptox: key must be 32 bytes (AES-256)")
)

// encryptGCM seals plaintext under key using AES-256-GCM with a fresh random
// nonce. Returns nonce||ciphertext||tag (no version prefix; version is
// applied by the Encryptor wrapper). Caller MUST pass a 32-byte key —
// length is enforced by the AES cipher constructor.
func encryptGCM(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("cryptox.encryptGCM: aes.NewCipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("cryptox.encryptGCM: cipher.NewGCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("cryptox.encryptGCM: read random nonce: %w", err)
	}

	// Seal appends ciphertext+tag onto its first arg, allowing us to lay
	// out nonce||ct||tag in a single allocation.
	out := make([]byte, len(nonce), len(nonce)+len(plaintext)+gcm.Overhead())
	copy(out, nonce)
	out = gcm.Seal(out, nonce, plaintext, nil)
	return out, nil
}

// decryptGCM opens nonce||ciphertext||tag under key using AES-256-GCM.
// Returns ErrCiphertextTooShort for blobs shorter than nonce+tag. Returns
// the GCM authentication failure (wrapped) for any tampered/wrong-key
// input — never accepts a tampered blob.
func decryptGCM(key, blob []byte) ([]byte, error) {
	if len(blob) < nonceLen+gcmTagLen {
		return nil, ErrCiphertextTooShort
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("cryptox.decryptGCM: aes.NewCipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("cryptox.decryptGCM: cipher.NewGCM: %w", err)
	}

	nonce := blob[:nonceLen]
	ct := blob[nonceLen:]
	plaintext, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		// Wrapped without leaking ct/nonce contents — message is generic
		// because GCM's failure mode (auth tag mismatch) is identical for
		// wrong-key, tampered-ct, and tampered-tag, by design.
		return nil, fmt.Errorf("cryptox.decryptGCM: gcm.Open: %w", err)
	}
	return plaintext, nil
}
