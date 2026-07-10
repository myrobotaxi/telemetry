package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// fakeES256Resolver resolves a single kid to a public key.
type fakeES256Resolver struct {
	kid string
	pub *ecdsa.PublicKey
}

func (f fakeES256Resolver) PublicKey(kid string) (*ecdsa.PublicKey, bool) {
	if kid == f.kid {
		return f.pub, true
	}
	return nil, false
}

// signES256 signs a token with the given ECDSA key and kid header.
func signES256(t *testing.T, key *ecdsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	if kid != "" {
		tok.Header["kid"] = kid
	}
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("signES256: %v", err)
	}
	return signed
}

func es256Claims() jwt.MapClaims {
	return jwt.MapClaims{
		"sub": "cmmgr4b1p0005l104ifpctlg8",
		"iss": "myrobotaxi",
		"aud": "telemetry",
		"iat": time.Now().Add(-time.Minute).Unix(),
		"exp": time.Now().Add(time.Hour).Unix(),
	}
}

// TestDualAlg_HS256StillWorks asserts legacy HS256 acceptance is unchanged
// when an ES256 resolver is also configured.
func TestDualAlg_HS256StillWorks(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	a := &JWTAuthenticator{
		secret: []byte(testSecret),
		issuer: "myrobotaxi", audience: "telemetry",
		es256: fakeES256Resolver{kid: "k1", pub: &key.PublicKey},
	}
	token := signToken(t, testSecret, jwt.MapClaims{
		"sub": "cuser", "iss": "myrobotaxi", "aud": "telemetry",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	got, err := a.ValidateToken(context.Background(), token)
	if err != nil {
		t.Fatalf("HS256 rejected under dual-alg: %v", err)
	}
	if got != "cuser" {
		t.Errorf("sub = %q", got)
	}
}

// TestDualAlg_ES256Accepted asserts a valid ES256 token verifies via the kid
// resolver.
func TestDualAlg_ES256Accepted(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	a := &JWTAuthenticator{
		secret: []byte(testSecret),
		issuer: "myrobotaxi", audience: "telemetry",
		es256: fakeES256Resolver{kid: "k1", pub: &key.PublicKey},
	}
	token := signES256(t, key, "k1", es256Claims())
	got, err := a.ValidateToken(context.Background(), token)
	if err != nil {
		t.Fatalf("valid ES256 rejected: %v", err)
	}
	if got != "cmmgr4b1p0005l104ifpctlg8" {
		t.Errorf("sub = %q", got)
	}
}

// TestDualAlg_ES256RejectedWhenDisabled asserts that without a resolver, ES256
// tokens are rejected (HS256-only legacy behaviour).
func TestDualAlg_ES256RejectedWhenDisabled(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	a := &JWTAuthenticator{secret: []byte(testSecret), issuer: "myrobotaxi", audience: "telemetry"}
	token := signES256(t, key, "k1", es256Claims())
	if _, err := a.ValidateToken(context.Background(), token); err == nil {
		t.Fatal("ES256 token accepted with no resolver configured")
	}
}

// TestDualAlg_UnknownKidRejected asserts a valid ES256 signature with an
// unknown kid is rejected.
func TestDualAlg_UnknownKidRejected(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	a := &JWTAuthenticator{
		secret: []byte(testSecret),
		issuer: "myrobotaxi", audience: "telemetry",
		es256: fakeES256Resolver{kid: "k1", pub: &key.PublicKey},
	}
	token := signES256(t, key, "k-other", es256Claims())
	if _, err := a.ValidateToken(context.Background(), token); err == nil {
		t.Fatal("token with unknown kid accepted")
	}
}

// TestDualAlg_MissingKidRejected asserts an ES256 token with no kid header is
// rejected (we require kid to select the verification key).
func TestDualAlg_MissingKidRejected(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	a := &JWTAuthenticator{
		secret: []byte(testSecret),
		issuer: "myrobotaxi", audience: "telemetry",
		es256: fakeES256Resolver{kid: "k1", pub: &key.PublicKey},
	}
	token := signES256(t, key, "", es256Claims())
	if _, err := a.ValidateToken(context.Background(), token); err == nil {
		t.Fatal("ES256 token without kid accepted")
	}
}

// TestDualAlg_AlgConfusion_PublicKeyAsHMAC is the core security assertion: an
// attacker takes the published ES256 public key and forges an HS256 token
// using the key's marshaled bytes as the HMAC secret. It MUST be rejected —
// the HMAC branch returns the real AUTH_SECRET, never the public key.
func TestDualAlg_AlgConfusion_PublicKeyAsHMAC(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	a := &JWTAuthenticator{
		secret: []byte(testSecret),
		issuer: "myrobotaxi", audience: "telemetry",
		es256: fakeES256Resolver{kid: "k1", pub: &key.PublicKey},
	}
	// Forge HS256 using the public key's X bytes as the HMAC key.
	forged := signToken(t, string(key.X.Bytes()), jwt.MapClaims{
		"sub": "attacker", "iss": "myrobotaxi", "aud": "telemetry",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	if _, err := a.ValidateToken(context.Background(), forged); err == nil {
		t.Fatal("alg-confusion (public key as HMAC secret) was accepted")
	}
}

// TestDualAlg_NoneRejected asserts the "none" algorithm is rejected under the
// method allowlist.
func TestDualAlg_NoneRejected(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	a := &JWTAuthenticator{
		secret: []byte(testSecret),
		issuer: "myrobotaxi", audience: "telemetry",
		es256: fakeES256Resolver{kid: "k1", pub: &key.PublicKey},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, es256Claims())
	unsigned, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("build none token: %v", err)
	}
	if _, err := a.ValidateToken(context.Background(), unsigned); err == nil {
		t.Fatal("none-alg token accepted")
	}
}
