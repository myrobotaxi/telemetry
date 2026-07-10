package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// appleRSAKey generates a test RSA key.
func appleRSAKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa: %v", err)
	}
	return key
}

// jwksServer serves a JWKS document for the given kid->public-key set.
func jwksServer(t *testing.T, keys map[string]*rsa.PublicKey) *httptest.Server {
	t.Helper()
	type jwk struct {
		Kty, Kid, N, E, Use, Alg string
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var out struct {
			Keys []jwk `json:"keys"`
		}
		for kid, pub := range keys {
			out.Keys = append(out.Keys, jwk{
				Kty: "RSA", Kid: kid, Use: "sig", Alg: "RS256",
				N: base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// signApple signs an Apple-style identity token with RS256 and a kid header.
func signApple(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign apple token: %v", err)
	}
	return signed
}

const testClientID = "app.myrobotaxi.ios"

func baseAppleClaims() jwt.MapClaims {
	now := time.Now()
	return jwt.MapClaims{
		"iss":            AppleIssuer,
		"aud":            testClientID,
		"sub":            "000123.apple.subject",
		"iat":            now.Add(-time.Minute).Unix(),
		"exp":            now.Add(time.Hour).Unix(),
		"email":          "owner@example.com",
		"email_verified": true,
	}
}

func TestAppleValidator_Valid(t *testing.T) {
	key := appleRSAKey(t)
	srv := jwksServer(t, map[string]*rsa.PublicKey{"k1": &key.PublicKey})
	v := NewAppleValidator(testClientID, srv.URL, nil)

	token := signApple(t, key, "k1", baseAppleClaims())
	claims, err := v.Validate(context.Background(), token, "")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if claims.Sub != "000123.apple.subject" {
		t.Errorf("sub = %q", claims.Sub)
	}
	if claims.Email != "owner@example.com" || !claims.EmailVerified {
		t.Errorf("email claims = %q verified=%v", claims.Email, claims.EmailVerified)
	}
}

func TestAppleValidator_Matrix(t *testing.T) {
	key := appleRSAKey(t)
	srv := jwksServer(t, map[string]*rsa.PublicKey{"k1": &key.PublicKey})
	v := NewAppleValidator(testClientID, srv.URL, nil)

	tests := []struct {
		name   string
		mutate func(jwt.MapClaims)
		kid    string
		method jwt.SigningMethod
	}{
		{"bad issuer", func(c jwt.MapClaims) { c["iss"] = "https://evil.example.com" }, "k1", nil},
		{"bad audience", func(c jwt.MapClaims) { c["aud"] = "app.someone.else" }, "k1", nil},
		{"expired", func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Hour).Unix() }, "k1", nil},
		{"missing sub", func(c jwt.MapClaims) { delete(c, "sub") }, "k1", nil},
		{"unknown kid", func(jwt.MapClaims) {}, "k-unknown", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := baseAppleClaims()
			tt.mutate(claims)
			token := signApple(t, key, tt.kid, claims)
			if _, err := v.Validate(context.Background(), token, ""); err == nil {
				t.Fatalf("expected rejection for %s", tt.name)
			}
		})
	}
}

// TestAppleValidator_AlgConfusion asserts a non-RS256 signing algorithm is
// rejected before the key callback runs (WithValidMethods allowlist).
func TestAppleValidator_AlgConfusion(t *testing.T) {
	key := appleRSAKey(t)
	srv := jwksServer(t, map[string]*rsa.PublicKey{"k1": &key.PublicKey})
	v := NewAppleValidator(testClientID, srv.URL, nil)

	// HS256 token forged with the (public) modulus bytes as the HMAC secret —
	// the classic RSA/HMAC confusion. Must be rejected by the method allowlist.
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, baseAppleClaims())
	tok.Header["kid"] = "k1"
	forged, err := tok.SignedString(key.N.Bytes())
	if err != nil {
		t.Fatalf("sign forged: %v", err)
	}
	if _, err := v.Validate(context.Background(), forged, ""); err == nil {
		t.Fatal("alg-confusion token was accepted")
	}
}

func TestAppleValidator_Nonce(t *testing.T) {
	key := appleRSAKey(t)
	srv := jwksServer(t, map[string]*rsa.PublicKey{"k1": &key.PublicKey})
	v := NewAppleValidator(testClientID, srv.URL, nil)

	claims := baseAppleClaims()
	claims["nonce"] = "abc123"
	token := signApple(t, key, "k1", claims)

	if _, err := v.Validate(context.Background(), token, "abc123"); err != nil {
		t.Fatalf("matching nonce rejected: %v", err)
	}
	if _, err := v.Validate(context.Background(), token, "wrong"); err == nil {
		t.Fatal("mismatched nonce accepted")
	}
	// Absent expected nonce -> token nonce is optional, accepted.
	if _, err := v.Validate(context.Background(), token, ""); err != nil {
		t.Fatalf("optional nonce rejected: %v", err)
	}
}

func TestAppleValidator_EmailVerifiedStringForm(t *testing.T) {
	key := appleRSAKey(t)
	srv := jwksServer(t, map[string]*rsa.PublicKey{"k1": &key.PublicKey})
	v := NewAppleValidator(testClientID, srv.URL, nil)

	claims := baseAppleClaims()
	claims["email_verified"] = "true" // Apple sometimes stringifies booleans
	claims["is_private_email"] = "true"
	token := signApple(t, key, "k1", claims)

	got, err := v.Validate(context.Background(), token, "")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !got.EmailVerified {
		t.Error("string 'true' email_verified not parsed as true")
	}
	if !got.IsPrivateEmail {
		t.Error("string 'true' is_private_email not parsed as true")
	}
}

// TestAppleKeyCache_Rotation asserts a previously-unseen kid triggers a
// re-fetch (Apple key rotation), exercised directly on the cache with the
// miss backoff disabled.
func TestAppleKeyCache_Rotation(t *testing.T) {
	key1 := appleRSAKey(t)
	key2 := appleRSAKey(t)
	served := map[string]*rsa.PublicKey{"k1": &key1.PublicKey}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		type jwk struct{ Kty, Kid, N, E string }
		var out struct {
			Keys []jwk `json:"keys"`
		}
		for kid, pub := range served {
			out.Keys = append(out.Keys, jwk{
				Kty: "RSA", Kid: kid,
				N: base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
				E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)

	cache := newAppleKeyCache(srv.URL, nil)
	cache.missBackfx = 0 // allow immediate miss-triggered refetch

	ctx := context.Background()
	if _, err := cache.key(ctx, "k1"); err != nil {
		t.Fatalf("k1: %v", err)
	}
	// Rotate: server now serves k2 as well; a request for k2 must refetch.
	served["k2"] = &key2.PublicKey
	if _, err := cache.key(ctx, "k2"); err != nil {
		t.Fatalf("k2 after rotation: %v", err)
	}
}

// sanity: ensure the JWK exponent decode rejects nonsense.
func TestRSAPublicFromJWK_BadExponent(t *testing.T) {
	if _, err := rsaPublicFromJWK(base64.RawURLEncoding.EncodeToString([]byte{1, 2, 3}), ""); err == nil {
		t.Fatal("expected error for empty exponent")
	}
}
