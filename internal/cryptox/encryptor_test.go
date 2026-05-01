package cryptox

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func newTestKeySet(t *testing.T, writeVersion byte, versions ...byte) *KeySet {
	t.Helper()
	keys := make(map[byte][]byte, len(versions))
	for _, v := range versions {
		key := make([]byte, keyLen)
		for i := range key {
			// Make each version's key distinguishable so we can detect
			// cross-version leakage in tests.
			key[i] = v
		}
		keys[v] = key
	}
	return &KeySet{writeVersion: writeVersion, keys: keys}
}

func TestEncryptor_RoundtripV1(t *testing.T) {
	enc, err := NewEncryptor(newTestKeySet(t, versionV1, versionV1))
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}

	tests := []string{
		"hello",
		"unicode: 日本語",
		strings.Repeat("a", 4096),
		"with newline\nand tab\t",
		`{"json":"payload","number":42}`,
	}

	for _, plaintext := range tests {
		t.Run(plaintext[:min(20, len(plaintext))], func(t *testing.T) {
			ct, err := enc.EncryptString(plaintext)
			if err != nil {
				t.Fatalf("EncryptString: %v", err)
			}
			pt, err := enc.DecryptString(ct)
			if err != nil {
				t.Fatalf("DecryptString: %v", err)
			}
			if pt != plaintext {
				t.Fatalf("roundtrip mismatch: got %q, want %q", pt, plaintext)
			}
		})
	}
}

func TestEncryptor_EmptyStringPassthrough(t *testing.T) {
	enc, err := NewEncryptor(newTestKeySet(t, versionV1, versionV1))
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	ct, err := enc.EncryptString("")
	if err != nil || ct != "" {
		t.Fatalf("EncryptString(\"\") = (%q, %v), want (\"\", nil)", ct, err)
	}
	pt, err := enc.DecryptString("")
	if err != nil || pt != "" {
		t.Fatalf("DecryptString(\"\") = (%q, %v), want (\"\", nil)", pt, err)
	}
}

func TestEncryptor_VersionByteIsActiveWriteVersion(t *testing.T) {
	// Both v1 and v2 keys present, write version is v2.
	ks := newTestKeySet(t, 2, 1, 2)
	enc, err := NewEncryptor(ks)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}

	ct, err := enc.EncryptString("payload")
	if err != nil {
		t.Fatalf("EncryptString: %v", err)
	}

	blob, err := base64.StdEncoding.DecodeString(ct)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if blob[0] != 2 {
		t.Fatalf("version byte = %d, want 2", blob[0])
	}
}

func TestEncryptor_DecryptCrossVersion(t *testing.T) {
	// Encrypt under v1, then load a KeySet that has both v1 (read-only)
	// and v2 (active write). Decrypt must succeed via the v1 key.
	v1Only := newTestKeySet(t, 1, 1)
	encV1, err := NewEncryptor(v1Only)
	if err != nil {
		t.Fatalf("NewEncryptor v1: %v", err)
	}
	ct, err := encV1.EncryptString("legacy payload")
	if err != nil {
		t.Fatalf("EncryptString v1: %v", err)
	}

	bothVersions := newTestKeySet(t, 2, 1, 2)
	encBoth, err := NewEncryptor(bothVersions)
	if err != nil {
		t.Fatalf("NewEncryptor both: %v", err)
	}
	pt, err := encBoth.DecryptString(ct)
	if err != nil {
		t.Fatalf("DecryptString cross-version: %v", err)
	}
	if pt != "legacy payload" {
		t.Fatalf("got %q, want \"legacy payload\"", pt)
	}
}

func TestEncryptor_UnknownKeyVersion(t *testing.T) {
	// Encrypt under v1, then try to decrypt with a KeySet that has only v2.
	// The version byte v1 has no matching key — must surface
	// ErrUnknownKeyVersion.
	v1Only := newTestKeySet(t, 1, 1)
	encV1, err := NewEncryptor(v1Only)
	if err != nil {
		t.Fatalf("NewEncryptor v1: %v", err)
	}
	ct, err := encV1.EncryptString("retired payload")
	if err != nil {
		t.Fatalf("EncryptString: %v", err)
	}

	v2Only := newTestKeySet(t, 2, 2)
	encV2, err := NewEncryptor(v2Only)
	if err != nil {
		t.Fatalf("NewEncryptor v2: %v", err)
	}
	if _, err := encV2.DecryptString(ct); !errors.Is(err, ErrUnknownKeyVersion) {
		t.Fatalf("expected ErrUnknownKeyVersion, got %v", err)
	}
}

func TestEncryptor_InvalidVersionByte(t *testing.T) {
	// Manually craft a ciphertext with version byte 0x00 (reserved as
	// invalid). Must surface ErrInvalidVersion.
	enc, err := NewEncryptor(newTestKeySet(t, versionV1, versionV1))
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}

	// Real ciphertext to get a well-formed nonce+tag, then overwrite the
	// version byte with 0x00.
	ct, err := enc.EncryptString("hello")
	if err != nil {
		t.Fatalf("EncryptString: %v", err)
	}
	blob, err := base64.StdEncoding.DecodeString(ct)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	blob[0] = 0x00
	tampered := base64.StdEncoding.EncodeToString(blob)

	if _, err := enc.DecryptString(tampered); !errors.Is(err, ErrInvalidVersion) {
		t.Fatalf("expected ErrInvalidVersion, got %v", err)
	}
}

func TestEncryptor_TooShortCiphertext(t *testing.T) {
	enc, err := NewEncryptor(newTestKeySet(t, versionV1, versionV1))
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}

	// 1 byte short of MinCiphertextLen.
	short := base64.StdEncoding.EncodeToString(make([]byte, MinCiphertextLen-1))
	if _, err := enc.DecryptString(short); !errors.Is(err, ErrCiphertextTooShort) {
		t.Fatalf("expected ErrCiphertextTooShort, got %v", err)
	}
}

func TestEncryptor_InvalidBase64(t *testing.T) {
	enc, err := NewEncryptor(newTestKeySet(t, versionV1, versionV1))
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}
	if _, err := enc.DecryptString("!!!not-base64!!!"); err == nil {
		t.Fatal("expected base64 decode error, got nil")
	}
}

func TestNewEncryptor_RejectsNilKeySet(t *testing.T) {
	if _, err := NewEncryptor(nil); err == nil {
		t.Fatal("expected error for nil KeySet, got nil")
	}
}

func TestNewEncryptor_RejectsKeySetWithoutWriteKey(t *testing.T) {
	// writeVersion=2 but only v1 in keys — should refuse.
	ks := &KeySet{writeVersion: 2, keys: map[byte][]byte{1: make([]byte, keyLen)}}
	if _, err := NewEncryptor(ks); err == nil {
		t.Fatal("expected error for missing write key, got nil")
	}
}

func TestEncryptor_NonceUniquenessSamePlaintext(t *testing.T) {
	enc, err := NewEncryptor(newTestKeySet(t, versionV1, versionV1))
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}

	// Encrypt the same plaintext twice — outputs MUST differ because the
	// random nonce is fresh per call. Catastrophic GCM failure mode is
	// nonce reuse (key recovery).
	const same = "deterministic input"
	a, err := enc.EncryptString(same)
	if err != nil {
		t.Fatalf("EncryptString a: %v", err)
	}
	b, err := enc.EncryptString(same)
	if err != nil {
		t.Fatalf("EncryptString b: %v", err)
	}
	if a == b {
		t.Fatal("ciphertexts must differ across calls (nonce reuse risk)")
	}
}

// FuzzDecryptString exercises DecryptString against arbitrary base64-ish
// input to catch regressions in the version + length gating before AES is
// invoked. Seeds cover the documented error paths (empty, too-short,
// invalid version 0x00, unknown version, malformed base64) plus a real
// roundtrip ciphertext. Run with `go test -fuzz=FuzzDecryptString` for
// extended exploration; the seed corpus runs on every `go test`.
func FuzzDecryptString(f *testing.F) {
	enc, err := NewEncryptor(newTestKeySet(&testing.T{}, versionV1, versionV1))
	if err != nil {
		f.Fatalf("NewEncryptor: %v", err)
	}

	// Seed corpus: documented error paths + a valid ciphertext.
	f.Add("")                                                    // empty sentinel
	f.Add("!!!not-base64!!!")                                    // base64 decode failure
	f.Add(base64.StdEncoding.EncodeToString([]byte{0x00}))       // too short, version 0x00
	f.Add(base64.StdEncoding.EncodeToString(make([]byte, 28)))   // one short of MinCiphertextLen
	f.Add(base64.StdEncoding.EncodeToString(make([]byte, 29)))   // exactly min, version 0x00 = invalid
	tooLong := make([]byte, 29)
	tooLong[0] = 0xFF
	f.Add(base64.StdEncoding.EncodeToString(tooLong))            // unknown version
	if ct, err := enc.EncryptString("seed"); err == nil {
		f.Add(ct)
	}

	f.Fuzz(func(t *testing.T, in string) {
		// Property: DecryptString must never panic on any input. Errors
		// are expected for most inputs; only valid ciphertexts decrypt
		// successfully.
		_, _ = enc.DecryptString(in)
	})
}
