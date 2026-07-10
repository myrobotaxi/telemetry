package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
)

// Keystore holds the identity module's ES256 signing material. It owns
// exactly one active signing key (used to mint access tokens) and a set of
// verification public keys keyed by `kid` (the active key plus any retired
// keys still trusted during a rotation window). The private key never leaves
// this struct — callers get only a Sign method and the public JWKS.
//
// The `kid` is the RFC 7638 JWK SHA-256 thumbprint of the public key, so it
// is stable, collision-resistant, and derivable from the key alone (no
// separate key-id bookkeeping).
type Keystore struct {
	signingKey *ecdsa.PrivateKey
	signingKID string
	verifiers  map[string]*ecdsa.PublicKey
	ephemeral  bool
}

// NewKeystoreFromPEM builds a Keystore from a PKCS#8 or SEC1 EC private-key
// PEM. The key MUST be on the P-256 curve (ES256). The returned keystore
// trusts exactly this one key for both signing and verification.
func NewKeystoreFromPEM(pemStr string) (*Keystore, error) {
	key, err := parseECPrivateKeyPEM(pemStr)
	if err != nil {
		return nil, fmt.Errorf("identity.NewKeystoreFromPEM: %w", err)
	}
	return newKeystore(key, false)
}

// NewEphemeralKeystore generates a throwaway P-256 key for local development
// (--dev). Tokens signed with it do not survive a restart. Callers MUST log
// loudly that an ephemeral key is in use; production requires a static key.
func NewEphemeralKeystore() (*Keystore, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("identity.NewEphemeralKeystore: generate: %w", err)
	}
	return newKeystore(key, true)
}

func newKeystore(key *ecdsa.PrivateKey, ephemeral bool) (*Keystore, error) {
	if key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("identity: signing key must be P-256 (ES256), got %s", key.Curve.Params().Name)
	}
	kid := jwkThumbprint(&key.PublicKey)
	return &Keystore{
		signingKey: key,
		signingKID: kid,
		verifiers:  map[string]*ecdsa.PublicKey{kid: &key.PublicKey},
		ephemeral:  ephemeral,
	}, nil
}

// SigningKID returns the `kid` of the active signing key.
func (k *Keystore) SigningKID() string { return k.signingKID }

// Ephemeral reports whether this keystore uses a throwaway dev key.
func (k *Keystore) Ephemeral() bool { return k.ephemeral }

// PrivateKey returns the active ES256 signing key. Kept unexported-in-spirit:
// only the in-package token signer calls it.
func (k *Keystore) privateKey() *ecdsa.PrivateKey { return k.signingKey }

// PublicKey resolves a verification key by `kid`. It satisfies the
// auth.ES256KeyResolver consumer interface so the shared JWT validator can
// verify ES256 tokens minted here. The bool is false for an unknown kid.
func (k *Keystore) PublicKey(kid string) (*ecdsa.PublicKey, bool) {
	pub, ok := k.verifiers[kid]
	return pub, ok
}

// JWKS returns the public JSON Web Key Set for GET /.well-known/jwks.json.
// Only public parameters are emitted; the private scalar never appears.
func (k *Keystore) JWKS() JWKS {
	keys := make([]JWK, 0, len(k.verifiers))
	for kid, pub := range k.verifiers {
		keys = append(keys, ecPublicJWK(pub, kid))
	}
	return JWKS{Keys: keys}
}

// parseECPrivateKeyPEM decodes a PEM block into an ECDSA private key,
// accepting both PKCS#8 ("PRIVATE KEY") and SEC1 ("EC PRIVATE KEY") encodings.
func parseECPrivateKeyPEM(pemStr string) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("no PEM block found (expected an EC PRIVATE KEY / PRIVATE KEY block)")
	}
	// SEC1.
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	// PKCS#8.
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse EC private key (tried SEC1 and PKCS#8): %w", err)
	}
	ecKey, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("PKCS#8 key is %T, not *ecdsa.PrivateKey", parsed)
	}
	return ecKey, nil
}

// jwkThumbprint computes the RFC 7638 SHA-256 thumbprint of an EC public key,
// base64url-encoded (no padding). The JSON is the required members in
// lexicographic order with no whitespace: {"crv","kty","x","y"}.
func jwkThumbprint(pub *ecdsa.PublicKey) string {
	x := coordBase64(pub.X)
	y := coordBase64(pub.Y)
	canonical := fmt.Sprintf(`{"crv":"P-256","kty":"EC","x":%q,"y":%q}`, x, y)
	sum := sha256.Sum256([]byte(canonical))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// coordBase64 encodes an EC coordinate as a fixed-width 32-byte big-endian
// value, base64url without padding (JWK "x"/"y" convention).
func coordBase64(coord *big.Int) string {
	const p256CoordBytes = 32
	buf := make([]byte, p256CoordBytes)
	coord.FillBytes(buf) // left-pads with zeros to exactly 32 bytes
	return base64.RawURLEncoding.EncodeToString(buf)
}

// ecPublicJWK builds the public JWK for an EC public key.
func ecPublicJWK(pub *ecdsa.PublicKey, kid string) JWK {
	return JWK{
		Kty: "EC",
		Crv: "P-256",
		X:   coordBase64(pub.X),
		Y:   coordBase64(pub.Y),
		Use: "sig",
		Alg: "ES256",
		Kid: kid,
	}
}
