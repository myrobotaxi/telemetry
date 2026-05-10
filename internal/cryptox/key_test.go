package cryptox

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// b64Key returns a base64-encoded 32-byte key with byte i = seed for tests.
func b64Key(seed byte) string {
	key := make([]byte, keyLen)
	for i := range key {
		key[i] = seed
	}
	return base64.StdEncoding.EncodeToString(key)
}

func TestLoadKeySetFromEnv_SingleKey(t *testing.T) {
	t.Setenv(envSingleKey, b64Key(0xAB))

	ks, err := LoadKeySetFromEnv()
	if err != nil {
		t.Fatalf("LoadKeySetFromEnv: %v", err)
	}
	if ks.WriteVersion() != versionV1 {
		t.Fatalf("writeVersion = %d, want %d", ks.WriteVersion(), versionV1)
	}
	if !ks.HasVersion(versionV1) {
		t.Fatal("expected v1 to be readable")
	}
}

func TestLoadKeySetFromEnv_VersionedShape(t *testing.T) {
	t.Setenv(envVersionedKeyPrefix+"1", b64Key(0x01))
	t.Setenv(envVersionedKeyPrefix+"2", b64Key(0x02))
	t.Setenv(envWriteVersion, "2")

	ks, err := LoadKeySetFromEnv()
	if err != nil {
		t.Fatalf("LoadKeySetFromEnv: %v", err)
	}
	if ks.WriteVersion() != 2 {
		t.Fatalf("writeVersion = %d, want 2", ks.WriteVersion())
	}
	if !ks.HasVersion(1) || !ks.HasVersion(2) {
		t.Fatal("expected v1 and v2 to be readable")
	}
}

func TestLoadKeySetFromEnv_Errors(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(t *testing.T)
		errSub string
	}{
		{
			name: "neither shape set",
			setup: func(t *testing.T) {
				t.Helper()
			},
			errSub: "neither",
		},
		{
			name: "both shapes set",
			setup: func(t *testing.T) {
				t.Helper()
				t.Setenv(envSingleKey, b64Key(0xAA))
				t.Setenv(envVersionedKeyPrefix+"1", b64Key(0xBB))
				t.Setenv(envWriteVersion, "1")
			},
			errSub: "pick one shape",
		},
		{
			name: "single key wrong length",
			setup: func(t *testing.T) {
				t.Helper()
				t.Setenv(envSingleKey, base64.StdEncoding.EncodeToString([]byte("too short")))
			},
			errSub: "32 bytes",
		},
		{
			name: "single key not base64",
			setup: func(t *testing.T) {
				t.Helper()
				t.Setenv(envSingleKey, "!!!not-base64!!!")
			},
			errSub: "base64",
		},
		{
			name: "versioned without write_version",
			setup: func(t *testing.T) {
				t.Helper()
				t.Setenv(envVersionedKeyPrefix+"1", b64Key(0x01))
			},
			errSub: "ENCRYPTION_WRITE_VERSION",
		},
		{
			name: "versioned with empty write_version",
			setup: func(t *testing.T) {
				t.Helper()
				t.Setenv(envVersionedKeyPrefix+"1", b64Key(0x01))
				t.Setenv(envWriteVersion, "")
			},
			errSub: "is required",
		},
		{
			name: "write_version refers to missing key",
			setup: func(t *testing.T) {
				t.Helper()
				t.Setenv(envVersionedKeyPrefix+"1", b64Key(0x01))
				t.Setenv(envWriteVersion, "5")
			},
			errSub: "no ENCRYPTION_KEY_V5",
		},
		{
			name: "write_version not an integer",
			setup: func(t *testing.T) {
				t.Helper()
				t.Setenv(envVersionedKeyPrefix+"1", b64Key(0x01))
				t.Setenv(envWriteVersion, "two")
			},
			errSub: "not a valid integer",
		},
		{
			name: "write_version out of byte range",
			setup: func(t *testing.T) {
				t.Helper()
				t.Setenv(envVersionedKeyPrefix+"1", b64Key(0x01))
				t.Setenv(envWriteVersion, "256")
			},
			errSub: "out of range",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear the relevant env vars (empty value = treated as not-set
			// by the loader, see key.go).
			t.Setenv(envSingleKey, "")
			t.Setenv(envWriteVersion, "")
			t.Setenv(envVersionedKeyPrefix+"1", "")
			t.Setenv(envVersionedKeyPrefix+"2", "")
			tt.setup(t)

			_, err := LoadKeySetFromEnv()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tt.errSub)
			}
			if !strings.Contains(err.Error(), tt.errSub) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.errSub)
			}
		})
	}
}

func TestKeySet_StringIsRedacted(t *testing.T) {
	t.Setenv(envSingleKey, b64Key(0xAB))

	ks, err := LoadKeySetFromEnv()
	if err != nil {
		t.Fatalf("LoadKeySetFromEnv: %v", err)
	}

	// Default %v formatting calls String — must not leak key material.
	got := fmt.Sprintf("%v", ks)
	if strings.Contains(got, "0xAB") || strings.Contains(got, b64Key(0xAB)) {
		t.Fatalf("KeySet String() leaked key material: %q", got)
	}
	if !strings.Contains(got, "redacted") {
		t.Fatalf("KeySet String() should mention 'redacted', got %q", got)
	}

	// Nil receiver must not panic.
	var nilKs *KeySet
	if got := nilKs.String(); !strings.Contains(got, "nil") {
		t.Fatalf("nil KeySet String should mention nil, got %q", got)
	}
}

func TestDecodeAndValidate_URLSafeBase64(t *testing.T) {
	// Build a key whose standard base64 contains '+' or '/' to confirm the
	// URL-safe fallback works. Use a key seeded to produce '+' in std b64.
	key := make([]byte, keyLen)
	for i := range key {
		key[i] = 0xFF
	}
	urlSafe := base64.URLEncoding.EncodeToString(key)
	got, err := decodeAndValidate(urlSafe)
	if err != nil {
		t.Fatalf("decodeAndValidate(URL-safe): %v", err)
	}
	if len(got) != keyLen {
		t.Fatalf("decoded length = %d, want %d", len(got), keyLen)
	}
}

func TestDecodeAndValidate_LengthError(t *testing.T) {
	bad := base64.StdEncoding.EncodeToString(make([]byte, 16)) // 128-bit, not 256
	_, err := decodeAndValidate(bad)
	if !errors.Is(err, ErrInvalidKeyLength) {
		t.Fatalf("expected ErrInvalidKeyLength, got %v", err)
	}
}

func TestKeySet_ReadableVersions(t *testing.T) {
	tests := []struct {
		name     string
		versions []byte
		want     []byte
	}{
		{"single v1", []byte{1}, []byte{1}},
		{"v1+v2 ascending", []byte{1, 2}, []byte{1, 2}},
		{"out of order keys are sorted", []byte{3, 1, 2}, []byte{1, 2, 3}},
		{"sparse range", []byte{2, 250}, []byte{2, 250}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keys := make(map[byte][]byte, len(tt.versions))
			for _, v := range tt.versions {
				keys[v] = make([]byte, keyLen)
			}
			ks := &KeySet{writeVersion: tt.versions[0], keys: keys}
			got := ks.ReadableVersions()
			if len(got) != len(tt.want) {
				t.Fatalf("len = %d, want %d (%v vs %v)", len(got), len(tt.want), got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("[%d] = %d, want %d (full: %v)", i, got[i], tt.want[i], got)
				}
			}
		})
	}
}
