// Package identity is the telemetry server's bounded identity module
// (MYR-193, docs/architecture/adr-001-identity-module.md). It owns native
// Sign in with Apple, ES256 access-token minting with a published JWKS, and
// rotating refresh tokens with family reuse-detection. The rest of the app
// consumes only the validated user identity (a user CUID) via the shared
// auth.JWTAuthenticator; nothing outside this package touches the signing
// key, the Apple validation, or the refresh-token store.
package identity

import "errors"

// Domain errors. Handlers map these to typed REST error envelopes; the raw
// errors never carry PII (an email or token) — only opaque ids.
var (
	// ErrInvalidAppleToken indicates the Apple identity token failed
	// validation (signature, issuer, audience, expiry, or claim shape).
	ErrInvalidAppleToken = errors.New("invalid apple identity token")

	// ErrEmailNotVerified indicates the Apple token carried an email that
	// Apple did not assert as verified, so it cannot be used for linkage.
	ErrEmailNotVerified = errors.New("apple email not verified")

	// ErrInvalidRefreshToken indicates the presented refresh token is
	// unknown, malformed, or naturally expired (benign — no family revoke).
	ErrInvalidRefreshToken = errors.New("invalid refresh token")

	// ErrRefreshReuseDetected indicates a spent or revoked refresh token was
	// presented — treated as theft. The whole family is revoked.
	ErrRefreshReuseDetected = errors.New("refresh token reuse detected")

	// ErrNoSigningKey indicates the keystore has no ES256 signing key, so no
	// access token can be minted (misconfiguration — module should be off).
	ErrNoSigningKey = errors.New("no ES256 signing key configured")

	// ErrUnknownKID indicates a token's `kid` did not resolve to a known
	// public key in the keystore.
	ErrUnknownKID = errors.New("unknown key id")
)
