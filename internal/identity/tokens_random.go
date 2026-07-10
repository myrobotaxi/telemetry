package identity

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"
)

// refreshTokenBytes sizes the opaque refresh token: 32 bytes = 256 bits of
// entropy, base64url-encoded to the client. Only its SHA-256 hash is stored.
const refreshTokenBytes = 32

// idRandomBytes sizes the random suffix of a cuid-shaped id (16 bytes -> 32
// hex chars + "c" prefix), matching the platform convention (see
// store.newRideRequestID / mask.newAuditID).
const idRandomBytes = 16

// newRefreshToken returns a fresh opaque refresh token and its SHA-256 hex
// digest. The raw token is returned to the client exactly once; the server
// persists only the hash (go_refresh_tokens.token_hash).
func newRefreshToken() (raw, hash string, err error) {
	b := make([]byte, refreshTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", "", fmt.Errorf("identity.newRefreshToken: read entropy: %w", err)
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, hashRefreshToken(raw), nil
}

// hashRefreshToken returns the lowercase SHA-256 hex digest of a raw refresh
// token — the value stored and looked up in go_refresh_tokens.
func hashRefreshToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// newID generates a cuid-shaped identifier for a Go-owned row (a go_users id
// or a refresh-token family_id). Mirrors store.newRideRequestID.
func newID() string {
	b := make([]byte, idRandomBytes)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("c%x", time.Now().UnixNano())
	}
	return "c" + hex.EncodeToString(b)
}
