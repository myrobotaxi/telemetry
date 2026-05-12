package cryptox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
)

// expectedFixtureSHA256 is the SHA-256 of the cross-implementation fixture
// as committed to both this repo and `myrobotaxi/react-frontend`. Encoded as
// the authoritative wire-format fingerprint shared between the Go and TS
// cryptox implementations: any drift here means the two ports have
// disagreed on AES-256-GCM ciphertext encoding and the fixture must be
// regenerated in lockstep across both repos.
//
// Source: docs/contracts/key-rotation.md ciphertext format §
//
// If this constant changes, also update:
//   - myrobotaxi/react-frontend/src/lib/cryptox/__fixtures__/cross-impl.json
//   - the SHA256 quoted in the MYR-62 PR body
const expectedFixtureSHA256 = "409ccb4a0fd6bff1bd1d97691e9fd17fccbf7f7171561a8a1ebc61b012c7fa8e"

// crossImplFixture mirrors the JSON shape produced by the TS port. Field
// names are snake_case to match the on-disk fixture; renaming requires a
// coordinated cross-repo update.
type crossImplFixture struct {
	Algorithm     string `json:"algorithm"`
	CiphertextB64 string `json:"ciphertext_b64"`
	KeyB64        string `json:"key_b64"`
	Plaintext     string `json:"plaintext"`
	Version       int    `json:"version"`
	WireFormat    string `json:"wire_format"`
}

// TestCrossImpl_FixtureSHA256 fails fast if the on-disk fixture has been
// edited or regenerated independently of its sibling in `myrobotaxi/react-frontend`.
// It is the byte-equality contract gate: TS encrypts under the same key
// and the Go port must decrypt the resulting blob without modification.
func TestCrossImpl_FixtureSHA256(t *testing.T) {
	raw, err := os.ReadFile("testdata/cross-impl.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	sum := sha256.Sum256(raw)
	got := hex.EncodeToString(sum[:])
	if got != expectedFixtureSHA256 {
		t.Fatalf("fixture SHA256 drift:\n got=%s\nwant=%s\n(regenerate the cross-impl fixture in lockstep across myrobotaxi/react-frontend and this repo)",
			got, expectedFixtureSHA256)
	}
}

// TestCrossImpl_DecryptTSCiphertext loads the TS-produced ciphertext blob
// and decrypts it under the fixture key using the Go AES-256-GCM
// implementation. A passing test proves the wire format (version byte,
// nonce length, tag length, base64 alphabet) is byte-identical between
// the Go and TS ports — neither side can drift without breaking this test.
//
// Decrypt-only by design: deterministic-nonce hooks would require
// exposing internals (rand source) just for this test, and one-way
// decryption is sufficient to prove byte compatibility because GCM auth
// fails closed on any wire-format mismatch.
func TestCrossImpl_DecryptTSCiphertext(t *testing.T) {
	raw, err := os.ReadFile("testdata/cross-impl.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	var fx crossImplFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse fixture JSON: %v", err)
	}

	if fx.Algorithm != "AES-256-GCM" {
		t.Fatalf("unexpected algorithm %q (want AES-256-GCM)", fx.Algorithm)
	}
	if fx.Version != int(versionV1) {
		t.Fatalf("unexpected version %d (want %d)", fx.Version, versionV1)
	}

	key, err := decodeAndValidate(fx.KeyB64)
	if err != nil {
		t.Fatalf("decode fixture key: %v", err)
	}

	ks := &KeySet{
		writeVersion: versionV1,
		keys:         map[byte][]byte{versionV1: key},
	}
	enc, err := NewEncryptor(ks)
	if err != nil {
		t.Fatalf("NewEncryptor: %v", err)
	}

	got, err := enc.DecryptString(fx.CiphertextB64)
	if err != nil {
		t.Fatalf("DecryptString: %v", err)
	}
	if got != fx.Plaintext {
		t.Fatalf("plaintext mismatch:\n got=%q\nwant=%q", got, fx.Plaintext)
	}
}
