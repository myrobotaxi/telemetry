package identity

// JWKS is a JSON Web Key Set — the public-key document served at
// GET /api/auth/.well-known/jwks.json (RFC 7517). Consumers (and the
// server's own dual-alg validator) resolve an access token's `kid` header to
// one of these keys to verify the ES256 signature.
type JWKS struct {
	Keys []JWK `json:"keys"`
}

// JWK is a single public JSON Web Key. Only the public EC parameters are
// emitted here — the private scalar `d` is never serialized. All fields are
// P0 (public by definition).
type JWK struct {
	Kty string `json:"kty"`           // key type — always "EC" for ES256
	Crv string `json:"crv"`           // curve — always "P-256"
	X   string `json:"x"`             // base64url big-endian x coordinate
	Y   string `json:"y"`             // base64url big-endian y coordinate
	Use string `json:"use,omitempty"` // "sig"
	Alg string `json:"alg,omitempty"` // "ES256"
	Kid string `json:"kid"`           // RFC 7638 thumbprint
}
