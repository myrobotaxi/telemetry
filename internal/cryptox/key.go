package cryptox

import (
	"encoding/base64"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Env var names for the two supported deployment shapes.
const (
	// envSingleKey is the single-key shorthand: ENCRYPTION_KEY=base64(32B).
	// Implies versionV1 is both the active write version and the only
	// readable version. Use this shape for v1 deployments before any key
	// rotation.
	envSingleKey = "ENCRYPTION_KEY"

	// envWriteVersion selects the active write version when running in
	// versioned shape (mutually exclusive with envSingleKey). Value is the
	// decimal version byte (e.g., "2").
	envWriteVersion = "ENCRYPTION_WRITE_VERSION"

	// envVersionedKeyPrefix is the prefix for versioned keys:
	// ENCRYPTION_KEY_V1=base64(32B), ENCRYPTION_KEY_V2=base64(32B), ...
	envVersionedKeyPrefix = "ENCRYPTION_KEY_V"
)

// KeySet holds the active write key plus 0..N retired-but-still-readable
// keys, indexed by version byte. Encrypt always uses writeVersion; Decrypt
// fans out by the version byte at the head of the ciphertext.
//
// Construct via LoadKeySetFromEnv. The zero value is unusable.
//
// KeySet deliberately does NOT implement Stringer (other than the redacted
// String) or MarshalJSON to prevent accidental key material leaks via
// slog.Any or json.Marshal. Default %v formatting prints "<KeySet:redacted>".
type KeySet struct {
	writeVersion byte
	keys         map[byte][]byte
}

// String returns a redacted placeholder. Defends against accidental
// slog.Any("ks", ks) or fmt.Sprintf("%v", ks) leaks of key material.
func (k *KeySet) String() string {
	if k == nil {
		return "<KeySet:nil>"
	}
	return "<KeySet:redacted>"
}

// WriteVersion reports the active version used by Encrypt. Used by
// observability code (e.g., metrics tagged with the write version).
func (k *KeySet) WriteVersion() byte {
	return k.writeVersion
}

// HasVersion reports whether the KeySet can decrypt ciphertexts written
// under the given version. Useful for fail-fast checks at startup or in
// tests; the runtime decryption path uses keyForVersion directly.
func (k *KeySet) HasVersion(v byte) bool {
	_, ok := k.keys[v]
	return ok
}

// ReadableVersions returns the set of version bytes the KeySet can
// decrypt, in ascending order. Used by the metrics layer to pre-register
// `cryptox_decrypt_total{version="N"}` labels at startup so a fresh
// /metrics scrape shows zeros for every readable version rather than
// missing labels (the latter races with operator dashboards during
// rotation). Order is deterministic so test assertions are stable.
func (k *KeySet) ReadableVersions() []byte {
	out := make([]byte, 0, len(k.keys))
	for v := range k.keys {
		out = append(out, v)
	}
	// Sort ascending. Bytes are small ints, so an in-place insertion
	// sort beats sort.Slice's reflective overhead at len ≤ 255.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// keyForVersion returns the key for a ciphertext version. Package-private
// because only the Encryptor's Decrypt path needs it; exposing it would
// invite callers to encrypt directly with raw keys, bypassing the
// version-prefix protocol.
func (k *KeySet) keyForVersion(v byte) ([]byte, bool) {
	key, ok := k.keys[v]
	return key, ok
}

// LoadKeySetFromEnv reads encryption keys from the process environment.
// Two shapes are accepted:
//
//   - Single-key shorthand: ENCRYPTION_KEY=base64(32B). writeVersion=v1,
//     readable versions={v1}.
//   - Versioned shape: ENCRYPTION_KEY_V1=base64(32B), ENCRYPTION_KEY_V2=...,
//     plus ENCRYPTION_WRITE_VERSION=N selecting the active write version.
//     All present versioned keys are added to the readable set.
//
// Empty env var values are treated as not-set so that a deployment cannot
// silently launch with an empty key. Returns ErrInvalidKeyLength if any
// key, after base64 decoding, is not exactly 32 bytes. Returns a wrapped
// error if both shapes are present (configuration ambiguity) or if
// neither is present.
func LoadKeySetFromEnv() (*KeySet, error) {
	single := os.Getenv(envSingleKey)
	hasSingle := single != ""

	versioned := versionedKeysFromEnv()

	switch {
	case hasSingle && len(versioned) > 0:
		return nil, fmt.Errorf("cryptox.LoadKeySetFromEnv: both %s and %sN keys are set; pick one shape", envSingleKey, envVersionedKeyPrefix)
	case hasSingle:
		key, err := decodeAndValidate(single)
		if err != nil {
			return nil, fmt.Errorf("cryptox.LoadKeySetFromEnv(%s): %w", envSingleKey, err)
		}
		return &KeySet{
			writeVersion: versionV1,
			keys:         map[byte][]byte{versionV1: key},
		}, nil
	case len(versioned) > 0:
		writeVer, err := writeVersionFromEnv()
		if err != nil {
			return nil, fmt.Errorf("cryptox.LoadKeySetFromEnv: %w", err)
		}
		ks := &KeySet{writeVersion: writeVer, keys: make(map[byte][]byte, len(versioned))}
		for v, b64 := range versioned {
			key, err := decodeAndValidate(b64)
			if err != nil {
				return nil, fmt.Errorf("cryptox.LoadKeySetFromEnv(%s%d): %w", envVersionedKeyPrefix, v, err)
			}
			ks.keys[v] = key
		}
		if _, ok := ks.keys[writeVer]; !ok {
			return nil, fmt.Errorf("cryptox.LoadKeySetFromEnv: %s=%d but no %s%d found", envWriteVersion, writeVer, envVersionedKeyPrefix, writeVer)
		}
		return ks, nil
	default:
		return nil, fmt.Errorf("cryptox.LoadKeySetFromEnv: neither %s nor %sN are set", envSingleKey, envVersionedKeyPrefix)
	}
}

// versionedKeysFromEnv scans os.Environ() for ENCRYPTION_KEY_V<N> entries
// and returns a map of version byte → base64 value.
func versionedKeysFromEnv() map[byte]string {
	out := make(map[byte]string)
	for _, kv := range os.Environ() {
		eq := strings.IndexByte(kv, '=')
		if eq < 0 {
			continue
		}
		name := kv[:eq]
		if !strings.HasPrefix(name, envVersionedKeyPrefix) {
			continue
		}
		suffix := name[len(envVersionedKeyPrefix):]
		n, err := strconv.Atoi(suffix)
		if err != nil || n < 1 || n > 255 {
			continue
		}
		val := kv[eq+1:]
		if val == "" {
			// Empty value is treated as not-set. Allows tests to clear a
			// versioned key with t.Setenv("ENCRYPTION_KEY_V1", "").
			continue
		}
		out[byte(n)] = val
	}
	return out
}

// writeVersionFromEnv parses ENCRYPTION_WRITE_VERSION as a decimal byte.
func writeVersionFromEnv() (byte, error) {
	raw := os.Getenv(envWriteVersion)
	if raw == "" {
		return 0, fmt.Errorf("%s is required when using %sN keys", envWriteVersion, envVersionedKeyPrefix)
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a valid integer: %w", envWriteVersion, raw, err)
	}
	if n < 1 || n > 255 {
		return 0, fmt.Errorf("%s=%d out of range (must be 1..255)", envWriteVersion, n)
	}
	return byte(n), nil
}

// decodeAndValidate decodes a base64-encoded key and validates it is
// exactly 32 bytes (AES-256). Accepts both standard and URL-safe base64
// to be tolerant of secret-store conventions.
func decodeAndValidate(b64 string) ([]byte, error) {
	b64 = strings.TrimSpace(b64)
	key, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		// Try URL-safe encoding before giving up.
		key, err = base64.URLEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("base64 decode: %w", err)
		}
	}
	if len(key) != keyLen {
		return nil, fmt.Errorf("%w: got %d bytes", ErrInvalidKeyLength, len(key))
	}
	return key, nil
}
