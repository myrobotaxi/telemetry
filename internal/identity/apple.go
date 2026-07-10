package identity

import (
	"context"
	"crypto/rsa"
	"fmt"
	"net/http"

	"github.com/golang-jwt/jwt/v5"
)

// AppleIssuer is the required `iss` on a Sign in with Apple identity token.
const AppleIssuer = "https://appleid.apple.com"

// AppleClaims is the validated subset of an Apple identity token this module
// consumes. Email/name are P1 (PII) and must never be logged.
type AppleClaims struct {
	Sub            string // Apple's stable per-user subject — the linkage key
	Email          string // may be empty (user hid it) or a private-relay address
	EmailVerified  bool   // Apple asserts the email is verified
	IsPrivateEmail bool   // the email is an Apple private-relay address
	Nonce          string // echoed nonce, if the client supplied one
}

// AppleValidator verifies Apple identity tokens: RS256 signature against
// Apple's rotating JWKS, plus issuer, audience (the native client id), and
// expiry/issued-at. It never contacts Apple beyond the JWKS fetch — there is
// no code-exchange step for the native identity-token flow.
type AppleValidator struct {
	keys     appleKeyResolver
	issuer   string
	audience string
}

// NewAppleValidator builds a validator whose `aud` check is clientID
// (APPLE_NATIVE_CLIENT_ID). keysURL/httpClient configure the JWKS cache; pass
// "" / nil for production defaults (Apple's endpoint, a 5s-timeout client).
func NewAppleValidator(clientID, keysURL string, httpClient *http.Client) *AppleValidator {
	return &AppleValidator{
		keys:     newAppleKeyCache(keysURL, httpClient),
		issuer:   AppleIssuer,
		audience: clientID,
	}
}

// Validate verifies the identity token and returns the accepted claims.
// expectedNonce, when non-empty, MUST match the token's nonce (the token's
// nonce is otherwise optional). Any failure returns ErrInvalidAppleToken.
func (v *AppleValidator) Validate(ctx context.Context, idToken, expectedNonce string) (AppleClaims, error) {
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
	)

	claims := jwt.MapClaims{}
	_, err := parser.ParseWithClaims(idToken, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, fmt.Errorf("apple token missing kid")
		}
		return v.appleKey(ctx, kid)
	})
	if err != nil {
		return AppleClaims{}, fmt.Errorf("identity.AppleValidator.Validate: %w: %w", ErrInvalidAppleToken, err)
	}

	sub, _ := claims["sub"].(string)
	if sub == "" {
		return AppleClaims{}, fmt.Errorf("identity.AppleValidator.Validate: %w: missing sub", ErrInvalidAppleToken)
	}

	out := AppleClaims{
		Sub:            sub,
		Email:          stringClaim(claims["email"]),
		EmailVerified:  boolClaim(claims["email_verified"]),
		IsPrivateEmail: boolClaim(claims["is_private_email"]),
		Nonce:          stringClaim(claims["nonce"]),
	}

	if expectedNonce != "" && out.Nonce != expectedNonce {
		return AppleClaims{}, fmt.Errorf("identity.AppleValidator.Validate: %w: nonce mismatch", ErrInvalidAppleToken)
	}
	return out, nil
}

// appleKey adapts the resolver to the jwt keyfunc return type.
func (v *AppleValidator) appleKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	return v.keys.key(ctx, kid)
}

// stringClaim coerces a JSON claim to string ("" if absent/wrong type).
func stringClaim(v any) string {
	s, _ := v.(string)
	return s
}

// boolClaim coerces an Apple claim to bool. Apple encodes email_verified /
// is_private_email as either a JSON bool or the strings "true"/"false".
func boolClaim(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t == "true"
	default:
		return false
	}
}
