package identity

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// AppleKeysURL is Apple's public JWKS endpoint for Sign in with Apple.
const AppleKeysURL = "https://appleid.apple.com/auth/keys"

// appleKeyResolver resolves an Apple signing key by `kid`. Defined at the
// consumer site (the token validator) so tests can supply a stub without a
// live network.
type appleKeyResolver interface {
	key(ctx context.Context, kid string) (*rsa.PublicKey, error)
}

// appleKeyCache fetches and caches Apple's JWKS. Apple rotates its signing
// keys, so on a cache miss (a `kid` we have not seen) the cache re-fetches —
// rate-limited to avoid hammering Apple when presented an unknown kid — and
// it proactively re-fetches once the cache is older than maxAge.
type appleKeyCache struct {
	url        string
	httpClient *http.Client
	maxAge     time.Duration // force refresh when cache is older than this
	missBackfx time.Duration // min interval between miss-triggered refreshes
	now        func() time.Time

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

// newAppleKeyCache builds an Apple JWKS cache. url defaults to AppleKeysURL;
// httpClient defaults to a client with a short timeout.
func newAppleKeyCache(url string, httpClient *http.Client) *appleKeyCache {
	if url == "" {
		url = AppleKeysURL
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	return &appleKeyCache{
		url:        url,
		httpClient: httpClient,
		maxAge:     6 * time.Hour,
		missBackfx: 1 * time.Minute,
		now:        time.Now,
	}
}

// key returns the RSA public key for kid, refreshing the cache if the key is
// absent (Apple key rotation) or the cache is stale.
func (c *appleKeyCache) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if k := c.keys[kid]; k != nil && c.now().Sub(c.fetchedAt) < c.maxAge {
		return k, nil
	}

	// Refresh on a stale cache or an unknown kid, but rate-limit the
	// miss-triggered path so an attacker spraying unknown kids can't force a
	// fetch per request.
	stale := c.keys == nil || c.now().Sub(c.fetchedAt) >= c.maxAge
	missAllowed := c.now().Sub(c.fetchedAt) >= c.missBackfx
	if stale || missAllowed {
		if err := c.refreshLocked(ctx); err != nil {
			// Serve a still-valid cached key on a refresh failure if we have
			// one; otherwise surface the error.
			if k := c.keys[kid]; k != nil {
				return k, nil
			}
			return nil, err
		}
	}

	if k := c.keys[kid]; k != nil {
		return k, nil
	}
	return nil, fmt.Errorf("identity: apple key %q not found: %w", kid, ErrUnknownKID)
}

// refreshLocked fetches the JWKS and replaces the cache. Caller holds c.mu.
func (c *appleKeyCache) refreshLocked(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, http.NoBody)
	if err != nil {
		return fmt.Errorf("identity: build apple jwks request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("identity: fetch apple jwks: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("identity: apple jwks status %d", resp.StatusCode)
	}

	var doc struct {
		Keys []struct {
			Kty string `json:"kty"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("identity: decode apple jwks: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.Kty != "RSA" || k.Kid == "" {
			continue
		}
		pub, err := rsaPublicFromJWK(k.N, k.E)
		if err != nil {
			continue // skip a malformed key rather than fail the whole set
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return fmt.Errorf("identity: apple jwks contained no usable RSA keys")
	}
	c.keys = keys
	c.fetchedAt = c.now()
	return nil
}

// rsaPublicFromJWK reconstructs an RSA public key from base64url JWK n/e.
func rsaPublicFromJWK(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, fmt.Errorf("decode n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		return nil, fmt.Errorf("decode e: %w", err)
	}
	e := new(big.Int).SetBytes(eBytes)
	if !e.IsInt64() || e.Int64() < 2 {
		return nil, fmt.Errorf("invalid RSA exponent")
	}
	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: int(e.Int64()),
	}, nil
}
