package push

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// testKeyPEM generates a throwaway P-256 key in PKCS#8 PEM form — the same
// shape as an Apple .p8 auth key. Generated per-run so no credential material
// is ever committed.
func testKeyPEM(t *testing.T) (string, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), key
}

func TestParseP8(t *testing.T) {
	validPEM, _ := testKeyPEM(t)

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	rsaDER, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		t.Fatalf("marshal rsa key: %v", err)
	}
	rsaPEM := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: rsaDER}))

	tests := []struct {
		name    string
		pem     string
		wantErr error
		wantOK  bool
	}{
		{name: "valid p8", pem: validPEM, wantOK: true},
		{name: "empty is not configured", pem: "", wantErr: ErrNoSigningKey},
		{name: "not pem", pem: "definitely not a pem"},
		{name: "pem but not pkcs8", pem: "-----BEGIN PRIVATE KEY-----\nZm9v\n-----END PRIVATE KEY-----\n"},
		{name: "wrong key type", pem: rsaPEM},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseP8(tt.pem)
			switch {
			case tt.wantOK:
				if err != nil {
					t.Fatalf("parseP8() error = %v, want nil", err)
				}
				if got == nil {
					t.Fatal("parseP8() key = nil, want key")
				}
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("parseP8() error = %v, want %v", err, tt.wantErr)
				}
			default:
				if err == nil {
					t.Fatal("parseP8() error = nil, want error")
				}
			}
		})
	}
}

// TestParseP8ErrorsNeverEchoKeyMaterial guards the P0 rule that key bytes never
// reach an error string (and from there, a log line).
func TestParseP8ErrorsNeverEchoKeyMaterial(t *testing.T) {
	validPEM, _ := testKeyPEM(t)
	// Corrupt the body so parsing fails after PEM decode succeeds.
	corrupt := "-----BEGIN PRIVATE KEY-----\nMIIBODNOTAKEY==\n-----END PRIVATE KEY-----\n"

	for _, in := range []string{corrupt, validPEM[:len(validPEM)/2]} {
		_, err := parseP8(in)
		if err == nil {
			continue
		}
		if len(err.Error()) > 80 {
			t.Fatalf("parseP8 error is suspiciously long (may embed key bytes): %q", err)
		}
	}
}

func TestTokenSourceMintsES256Claims(t *testing.T) {
	_, key := testKeyPEM(t)
	ts := newTokenSource(key, "ABC1234567", "NFKX777598")
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	ts.now = func() time.Time { return now }

	got, err := ts.token()
	if err != nil {
		t.Fatalf("token() error = %v", err)
	}

	parsed, err := jwt.Parse(got, func(*jwt.Token) (any, error) { return &key.PublicKey, nil })
	if err != nil {
		t.Fatalf("parse minted token: %v", err)
	}
	if alg := parsed.Method.Alg(); alg != "ES256" {
		t.Errorf("alg = %q, want ES256", alg)
	}
	if kid, _ := parsed.Header["kid"].(string); kid != "ABC1234567" {
		t.Errorf("kid = %q, want ABC1234567", kid)
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok {
		t.Fatalf("claims type = %T, want jwt.MapClaims", parsed.Claims)
	}
	if iss, _ := claims["iss"].(string); iss != "NFKX777598" {
		t.Errorf("iss = %q, want NFKX777598", iss)
	}
	iat, _ := claims["iat"].(float64)
	if int64(iat) != now.Unix() {
		t.Errorf("iat = %v, want %v", int64(iat), now.Unix())
	}
}

func TestTokenSourceCaching(t *testing.T) {
	_, key := testKeyPEM(t)
	base := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		advance  time.Duration
		wantSame bool
	}{
		{name: "reuses within ttl", advance: tokenTTL - time.Minute, wantSame: true},
		{name: "remints after ttl", advance: tokenTTL + time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := base
			ts := newTokenSource(key, "ABC1234567", "NFKX777598")
			ts.now = func() time.Time { return now }

			first, err := ts.token()
			if err != nil {
				t.Fatalf("first token() error = %v", err)
			}
			now = base.Add(tt.advance)
			second, err := ts.token()
			if err != nil {
				t.Fatalf("second token() error = %v", err)
			}

			if same := first == second; same != tt.wantSame {
				t.Errorf("token reused = %v, want %v", same, tt.wantSame)
			}
		})
	}
}
