package identity

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// TokenMinter mints ES256 access tokens signed by the Keystore's active key.
// The tokens are validated by the shared auth.JWTAuthenticator (dual-alg),
// which resolves the `kid` header to the public key via the Keystore.
type TokenMinter struct {
	keystore *Keystore
	issuer   string
	audience string
	ttl      time.Duration
	now      func() time.Time // injectable clock for tests
}

// NewTokenMinter builds a minter. issuer/audience MUST match the values the
// dual-alg validator checks (config auth.token_issuer / auth.token_audience —
// "myrobotaxi" / "telemetry") so ES256 and legacy HS256 tokens validate
// identically. ttl is the access-token lifetime (~1h).
func NewTokenMinter(keystore *Keystore, issuer, audience string, ttl time.Duration) *TokenMinter {
	return &TokenMinter{
		keystore: keystore,
		issuer:   issuer,
		audience: audience,
		ttl:      ttl,
		now:      time.Now,
	}
}

// MintAccessToken signs a fresh ES256 access token for userID (a user CUID).
// It returns the compact JWT and the lifetime in whole seconds (the
// `expiresIn` the iOS client uses to schedule a proactive refresh).
func (m *TokenMinter) MintAccessToken(userID string) (token string, expiresIn int, err error) {
	if m.keystore == nil || m.keystore.privateKey() == nil {
		return "", 0, fmt.Errorf("identity.MintAccessToken: %w", ErrNoSigningKey)
	}
	now := m.now().UTC()
	claims := jwt.RegisteredClaims{
		Subject:   userID,
		Issuer:    m.issuer,
		Audience:  jwt.ClaimStrings{m.audience},
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(m.ttl)),
	}
	jwtToken := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	jwtToken.Header["kid"] = m.keystore.SigningKID()

	signed, err := jwtToken.SignedString(m.keystore.privateKey())
	if err != nil {
		return "", 0, fmt.Errorf("identity.MintAccessToken: sign: %w", err)
	}
	return signed, int(m.ttl.Seconds()), nil
}
