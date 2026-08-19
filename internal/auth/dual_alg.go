package auth

import (
	"crypto/ecdsa"
	"fmt"

	"github.com/golang-jwt/jwt/v5"
)

// ES256KeyResolver resolves an ES256 verification public key by its `kid`
// header. It is the consumer-site view of the identity module's keystore
// (identity.Keystore.PublicKey satisfies it). Injected into the
// JWTAuthenticator so the single validator can verify BOTH legacy HS256
// (shared-secret) tokens AND new ES256 (locally-minted) tokens — the
// dual-algorithm validation of ADR-001 §3.
type ES256KeyResolver interface {
	PublicKey(kid string) (*ecdsa.PublicKey, bool)
}

// Option configures a JWTAuthenticator at construction.
type Option func(*JWTAuthenticator)

// WithES256Resolver enables ES256 verification against the given resolver.
// When unset, the validator remains HS256-only and rejects ES256 tokens.
func WithES256Resolver(r ES256KeyResolver) Option {
	return func(a *JWTAuthenticator) { a.es256 = r }
}

// WithActivityStamper enables MYR-592 last-seen tracking: every successful
// ValidateToken records that its subject was seen, throttled to roughly one
// write per account-hour. When unset, nothing is recorded and the inactivity
// sweeper — which requires positive evidence of an owner before it will act —
// suspends nothing.
func WithActivityStamper(s *UserActivityStamper) Option {
	return func(a *JWTAuthenticator) {
		// A typed-nil pointer stored into the interface would make the
		// `a.activity != nil` guard in ValidateToken true while every call
		// panicked, so the nil case is filtered here rather than in the hot
		// path. (Stamp itself is also nil-receiver safe; belt and braces,
		// because this is inside authentication.)
		if s == nil {
			return
		}
		a.activity = s
	}
}

// validMethods returns the algorithm allowlist for this authenticator. HS256
// is always allowed (legacy shared-secret tokens); ES256 is added only when
// an ES256 resolver is configured. `none` and every other alg are rejected
// by jwt.WithValidMethods before the key callback runs.
func (a *JWTAuthenticator) validMethods() []string {
	if a.es256 != nil {
		return []string{"HS256", "ES256"}
	}
	return []string{"HS256"}
}

// keyFunc is the type-driven jwt key callback that pins the key material to
// the token's signing-method TYPE, making alg-confusion impossible:
//
//   - an HMAC-typed token gets the []byte shared secret and only that (an
//     attacker who signs an HS256 token with the published ES256 public key
//     as the HMAC key fails — this branch returns AUTH_SECRET, never a public
//     key);
//   - an ECDSA-typed token gets an *ecdsa.PublicKey resolved by `kid` and
//     only that (an ES256 token can never be verified with the HMAC secret).
func (a *JWTAuthenticator) keyFunc(t *jwt.Token) (any, error) {
	switch t.Method.(type) {
	case *jwt.SigningMethodHMAC:
		return a.secret, nil
	case *jwt.SigningMethodECDSA:
		if a.es256 == nil {
			return nil, fmt.Errorf("ES256 not enabled: %v", t.Header["alg"])
		}
		if t.Method.Alg() != "ES256" {
			return nil, fmt.Errorf("unsupported ECDSA algorithm: %v", t.Method.Alg())
		}
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, fmt.Errorf("ES256 token missing kid header")
		}
		pub, ok := a.es256.PublicKey(kid)
		if !ok {
			return nil, fmt.Errorf("unknown kid: %s", kid)
		}
		return pub, nil
	default:
		return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
	}
}
