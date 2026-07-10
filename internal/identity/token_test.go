package identity

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestMintAccessToken_ClaimsAndHeader(t *testing.T) {
	ks, err := NewKeystoreFromPEM(testKeyPEM(t))
	if err != nil {
		t.Fatalf("keystore: %v", err)
	}
	minter := NewTokenMinter(ks, "myrobotaxi", "telemetry", time.Hour)
	fixed := time.Date(2030, 7, 10, 12, 0, 0, 0, time.UTC)
	minter.now = func() time.Time { return fixed }

	token, expiresIn, err := minter.MintAccessToken("cmmgr4b1p0005l104ifpctlg8")
	if err != nil {
		t.Fatalf("MintAccessToken: %v", err)
	}
	if expiresIn != 3600 {
		t.Errorf("expiresIn = %d, want 3600", expiresIn)
	}

	// Verify via the keystore's own public key resolver.
	parsed, err := jwt.Parse(token, func(tok *jwt.Token) (any, error) {
		if tok.Method.Alg() != "ES256" {
			t.Fatalf("alg = %s, want ES256", tok.Method.Alg())
		}
		kid, _ := tok.Header["kid"].(string)
		if kid != ks.SigningKID() {
			t.Fatalf("kid header = %q, want %q", kid, ks.SigningKID())
		}
		pub, ok := ks.PublicKey(kid)
		if !ok {
			t.Fatalf("kid did not resolve")
		}
		return pub, nil
	}, jwt.WithValidMethods([]string{"ES256"}), jwt.WithTimeFunc(func() time.Time { return fixed }))
	if err != nil {
		t.Fatalf("parse minted token: %v", err)
	}

	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatal("claims not MapClaims")
	}
	if claims["sub"] != "cmmgr4b1p0005l104ifpctlg8" {
		t.Errorf("sub = %v", claims["sub"])
	}
	if claims["iss"] != "myrobotaxi" {
		t.Errorf("iss = %v", claims["iss"])
	}
	// aud is an array in the token.
	if aud, _ := parsed.Claims.GetAudience(); len(aud) != 1 || aud[0] != "telemetry" {
		t.Errorf("aud = %v", aud)
	}
	exp, _ := parsed.Claims.GetExpirationTime()
	if !exp.Equal(fixed.Add(time.Hour)) {
		t.Errorf("exp = %v, want %v", exp.Time, fixed.Add(time.Hour))
	}
}

func TestMintAccessToken_NoSigningKey(t *testing.T) {
	minter := NewTokenMinter(&Keystore{}, "iss", "aud", time.Hour)
	if _, _, err := minter.MintAccessToken("u"); err == nil {
		t.Fatal("expected ErrNoSigningKey")
	}
}
