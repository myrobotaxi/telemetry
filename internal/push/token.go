package push

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// tokenTTL is how long a minted APNs provider token is reused before a fresh
// one is signed. Apple rejects tokens older than 60 minutes (ExpiredProviderToken)
// and throttles providers that mint a new token more often than once every 20
// minutes (TooManyProviderTokenUpdates), so 40 minutes sits in the middle of the
// only safe window.
const tokenTTL = 40 * time.Minute

// ErrNoSigningKey is returned when an APNs signing key was expected but the
// configured PEM was empty. Callers treat it as "push is not configured".
var ErrNoSigningKey = errors.New("push: no APNs signing key configured")

// parseP8 decodes an APNs .p8 auth key (PKCS#8 PEM, as downloaded from the
// Apple Developer portal) into its ECDSA private key.
//
// The PEM content is P0 secret material sourced from the APNS_KEY_P8
// environment variable — it is never logged and never returned in an error
// message.
func parseP8(pemContent string) (*ecdsa.PrivateKey, error) {
	if pemContent == "" {
		return nil, ErrNoSigningKey
	}

	block, _ := pem.Decode([]byte(pemContent))
	if block == nil {
		return nil, errors.New("push: APNs key is not valid PEM")
	}

	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		// Deliberately not wrapping: x509 parse errors can echo key bytes.
		return nil, errors.New("push: APNs key is not a valid PKCS#8 key")
	}

	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("push: APNs key is %T, want *ecdsa.PrivateKey", parsed)
	}
	return key, nil
}

// tokenSource mints and caches the ES256 provider token APNs expects in the
// `authorization: bearer` header. Safe for concurrent use.
type tokenSource struct {
	key    *ecdsa.PrivateKey
	keyID  string
	teamID string
	now    func() time.Time // injectable clock for tests

	mu       sync.Mutex
	cached   string
	mintedAt time.Time
}

// newTokenSource builds a tokenSource from an already-parsed signing key.
func newTokenSource(key *ecdsa.PrivateKey, keyID, teamID string) *tokenSource {
	return &tokenSource{key: key, keyID: keyID, teamID: teamID, now: time.Now}
}

// token returns a provider token, minting a fresh one when the cached token is
// older than tokenTTL.
func (t *tokenSource) token() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()
	if t.cached != "" && now.Sub(t.mintedAt) < tokenTTL {
		return t.cached, nil
	}

	signed, err := t.mint(now)
	if err != nil {
		return "", err
	}
	t.cached, t.mintedAt = signed, now
	return signed, nil
}

// mint signs a new ES256 JWT. APNs requires exactly two claims (iss = team id,
// iat = issue time) and a `kid` header naming the key.
func (t *tokenSource) mint(now time.Time) (string, error) {
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, jwt.MapClaims{
		"iss": t.teamID,
		"iat": now.Unix(),
	})
	tok.Header["kid"] = t.keyID

	signed, err := tok.SignedString(t.key)
	if err != nil {
		return "", fmt.Errorf("push: sign APNs provider token: %w", err)
	}
	return signed, nil
}
