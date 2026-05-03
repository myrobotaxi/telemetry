package cryptox

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"
)

func mustRandKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, keyLen)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		t.Fatalf("rand key: %v", err)
	}
	return key
}

func TestEncryptGCM_Roundtrip(t *testing.T) {
	tests := []struct {
		name      string
		plaintext []byte
	}{
		{"empty", []byte{}},
		{"short ascii", []byte("hello")},
		{"binary", []byte{0x00, 0xFF, 0x10, 0xAB, 0x00}},
		{"longer than block size", bytes.Repeat([]byte("abc123"), 100)},
		{"exact AES block (16 bytes)", bytes.Repeat([]byte("a"), 16)},
		{"single byte", []byte{0x42}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := mustRandKey(t)
			ct, err := encryptGCM(key, tt.plaintext)
			if err != nil {
				t.Fatalf("encryptGCM: %v", err)
			}
			pt, err := decryptGCM(key, ct)
			if err != nil {
				t.Fatalf("decryptGCM: %v", err)
			}
			if !bytes.Equal(pt, tt.plaintext) {
				t.Fatalf("roundtrip mismatch: got %v want %v", pt, tt.plaintext)
			}
		})
	}
}

func TestEncryptGCM_NonceUniqueness(t *testing.T) {
	// Encrypting the same plaintext under the same key must produce
	// different ciphertexts because the random nonce is unique per call.
	// Catastrophic GCM failure mode is nonce reuse → key recovery.
	key := mustRandKey(t)
	plaintext := []byte("the same thing every time")
	const N = 1000

	seen := make(map[string]struct{}, N)
	for i := 0; i < N; i++ {
		ct, err := encryptGCM(key, plaintext)
		if err != nil {
			t.Fatalf("iter %d: encryptGCM: %v", i, err)
		}
		nonce := string(ct[:nonceLen])
		if _, dup := seen[nonce]; dup {
			t.Fatalf("nonce collision at iter %d", i)
		}
		seen[nonce] = struct{}{}
	}
}

func TestDecryptGCM_TamperedCiphertext(t *testing.T) {
	key := mustRandKey(t)
	ct, err := encryptGCM(key, []byte("genuine"))
	if err != nil {
		t.Fatalf("encryptGCM: %v", err)
	}

	// Flip one byte in the ciphertext body (between nonce and tag).
	// GCM auth check must fail.
	if len(ct) <= nonceLen+gcmTagLen {
		t.Fatalf("ciphertext too short to tamper: %d bytes", len(ct))
	}
	// Pick a byte in the middle of the ct+tag region.
	tamperIdx := nonceLen + (len(ct)-nonceLen-gcmTagLen)/2
	tampered := append([]byte(nil), ct...)
	tampered[tamperIdx] ^= 0xFF

	if _, err := decryptGCM(key, tampered); err == nil {
		t.Fatal("expected error on tampered ciphertext, got nil")
	}
}

func TestDecryptGCM_TamperedAuthTag(t *testing.T) {
	key := mustRandKey(t)
	ct, err := encryptGCM(key, []byte("genuine"))
	if err != nil {
		t.Fatalf("encryptGCM: %v", err)
	}

	// Flip the last byte (within the GCM auth tag).
	tampered := append([]byte(nil), ct...)
	tampered[len(tampered)-1] ^= 0x01

	if _, err := decryptGCM(key, tampered); err == nil {
		t.Fatal("expected error on tampered auth tag, got nil")
	}
}

func TestDecryptGCM_WrongKey(t *testing.T) {
	keyA := mustRandKey(t)
	keyB := mustRandKey(t)
	ct, err := encryptGCM(keyA, []byte("secret"))
	if err != nil {
		t.Fatalf("encryptGCM: %v", err)
	}
	if _, err := decryptGCM(keyB, ct); err == nil {
		t.Fatal("expected error decrypting with wrong key, got nil")
	}
}

func TestDecryptGCM_ShortInput(t *testing.T) {
	key := mustRandKey(t)
	tests := [][]byte{
		nil,
		{},
		{0x00},
		bytes.Repeat([]byte{0xAB}, nonceLen),               // nonce only
		bytes.Repeat([]byte{0xAB}, nonceLen+gcmTagLen-1),   // 1 byte short of min
	}
	for i, blob := range tests {
		if _, err := decryptGCM(key, blob); !errors.Is(err, ErrCiphertextTooShort) {
			t.Fatalf("case %d: expected ErrCiphertextTooShort, got %v", i, err)
		}
	}
}

func TestDecryptGCM_InvalidKeySize(t *testing.T) {
	// AES.NewCipher rejects keys that are not 16/24/32 bytes. The wrapper
	// must propagate that error rather than panic.
	for _, n := range []int{0, 15, 17, 31, 33, 48} {
		bad := make([]byte, n)
		ct := bytes.Repeat([]byte{0xAB}, MinCiphertextLen)
		if _, err := decryptGCM(bad, ct); err == nil {
			t.Fatalf("len=%d: expected error from AES.NewCipher, got nil", n)
		}
	}
}
