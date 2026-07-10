package identity

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"testing"
)

// testKeyPEM generates a P-256 PKCS#8 PEM for tests.
func testKeyPEM(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestNewKeystoreFromPEM_RoundTrip(t *testing.T) {
	ks, err := NewKeystoreFromPEM(testKeyPEM(t))
	if err != nil {
		t.Fatalf("NewKeystoreFromPEM: %v", err)
	}
	if ks.SigningKID() == "" {
		t.Fatal("empty kid")
	}
	if ks.Ephemeral() {
		t.Fatal("static key reported ephemeral")
	}
	// The signing kid resolves to a public key.
	pub, ok := ks.PublicKey(ks.SigningKID())
	if !ok || pub == nil {
		t.Fatal("signing kid did not resolve to a public key")
	}
	// An unknown kid does not.
	if _, ok := ks.PublicKey("nope"); ok {
		t.Fatal("unknown kid resolved")
	}
}

func TestKeystore_SEC1PEMAccepted(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	der, err := x509.MarshalECPrivateKey(key) // SEC1 "EC PRIVATE KEY"
	if err != nil {
		t.Fatalf("marshal sec1: %v", err)
	}
	sec1 := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
	if _, err := NewKeystoreFromPEM(sec1); err != nil {
		t.Fatalf("SEC1 PEM rejected: %v", err)
	}
}

func TestKeystore_RejectsNonP256(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate p384: %v", err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	p384 := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if _, err := NewKeystoreFromPEM(p384); err == nil {
		t.Fatal("expected P-384 key to be rejected")
	}
}

func TestKeystore_RejectsGarbage(t *testing.T) {
	if _, err := NewKeystoreFromPEM("not a pem"); err == nil {
		t.Fatal("expected error for non-PEM input")
	}
}

func TestEphemeralKeystore(t *testing.T) {
	ks, err := NewEphemeralKeystore()
	if err != nil {
		t.Fatalf("NewEphemeralKeystore: %v", err)
	}
	if !ks.Ephemeral() {
		t.Fatal("ephemeral keystore not flagged ephemeral")
	}
	if ks.SigningKID() == "" {
		t.Fatal("empty kid")
	}
}

func TestKeystore_JWKSShape(t *testing.T) {
	ks, err := NewKeystoreFromPEM(testKeyPEM(t))
	if err != nil {
		t.Fatalf("keystore: %v", err)
	}
	jwks := ks.JWKS()
	if len(jwks.Keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(jwks.Keys))
	}
	k := jwks.Keys[0]
	if k.Kty != "EC" || k.Crv != "P-256" || k.Alg != "ES256" || k.Use != "sig" {
		t.Errorf("unexpected JWK metadata: %+v", k)
	}
	if k.Kid != ks.SigningKID() {
		t.Errorf("JWK kid %q != signing kid %q", k.Kid, ks.SigningKID())
	}
	if k.X == "" || k.Y == "" {
		t.Error("JWK missing coordinates")
	}
}

func TestJWKThumbprint_Deterministic(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	a := jwkThumbprint(&key.PublicKey)
	b := jwkThumbprint(&key.PublicKey)
	if a != b {
		t.Errorf("thumbprint not deterministic: %q != %q", a, b)
	}
	if a == "" {
		t.Error("empty thumbprint")
	}
}
