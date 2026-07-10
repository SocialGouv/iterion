package oidc

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// jwksVerifyAlgs is the allowlist of asymmetric signing algorithms accepted for
// ID-token verification. Critically, this EXCLUDES "none" and the HMAC family:
// without an allowlist an attacker could present an HS256 token signed with the
// (public) JWKS key as the HMAC secret, or an alg=none token — the classic
// JWS algorithm-confusion attacks.
var jwksVerifyAlgs = []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512"}

// jwksTTL bounds how long a fetched key set is reused before a refresh. A kid
// miss forces an immediate refetch regardless (handles key rotation between
// refreshes), throttled by jwksMinRefetch so a bogus kid can't hammer the IdP.
const (
	jwksTTL        = time.Hour
	jwksMinRefetch = time.Minute
	jwksMaxBytes   = 512 << 10 // 512 KiB cap on a JWKS document
)

// idTokenClaims is the subset of the OIDC ID token we consume. Audience/expiry
// are validated by the jwt parser via RegisteredClaims; the rest are read after
// the signature + standard-claim checks pass.
type idTokenClaims struct {
	Subject       string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
	jwt.RegisteredClaims
}

// jwkSet / jwk mirror the RFC 7517 JSON Web Key Set wire shape (only the fields
// we need to reconstruct RSA + EC public keys).
type jwkSet struct {
	Keys []jwk `json:"keys"`
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// jwksCache caches the public keys for one jwks_uri, keyed by kid, with a TTL
// and a kid-miss refetch (throttled). Safe for concurrent use.
type jwksCache struct {
	url    string
	client *http.Client

	mu          sync.Mutex
	byKID       map[string]crypto.PublicKey
	fetchedAt   time.Time
	lastRefetch time.Time
}

func newJWKSCache(url string, client *http.Client) *jwksCache {
	return &jwksCache{url: url, client: client}
}

// keyForKID returns the public key for kid, fetching/refreshing the set when
// the cache is empty, stale, or misses the kid (rotation). kid may be empty
// (single-key sets): then the sole key is returned.
func (c *jwksCache) keyForKID(ctx context.Context, kid string) (crypto.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	fresh := c.byKID != nil && now.Sub(c.fetchedAt) < jwksTTL
	if fresh {
		if k, ok := c.lookupLocked(kid); ok {
			return k, nil
		}
		// kid miss on a fresh set → maybe a rotation; refetch (throttled).
		if now.Sub(c.lastRefetch) < jwksMinRefetch {
			return nil, fmt.Errorf("oidc/jwks: no key for kid")
		}
	}
	if err := c.refetchLocked(ctx); err != nil {
		return nil, err
	}
	if k, ok := c.lookupLocked(kid); ok {
		return k, nil
	}
	return nil, fmt.Errorf("oidc/jwks: no key for kid")
}

func (c *jwksCache) lookupLocked(kid string) (crypto.PublicKey, bool) {
	if kid != "" {
		k, ok := c.byKID[kid]
		return k, ok
	}
	// No kid in the token header: only safe when the set has exactly one key.
	if len(c.byKID) == 1 {
		for _, k := range c.byKID {
			return k, true
		}
	}
	return nil, false
}

func (c *jwksCache) refetchLocked(ctx context.Context) error {
	c.lastRefetch = time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return fmt.Errorf("oidc/jwks: build req: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("oidc/jwks: fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("oidc/jwks: fetch %d", resp.StatusCode)
	}
	var set jwkSet
	if err := json.NewDecoder(io.LimitReader(resp.Body, jwksMaxBytes)).Decode(&set); err != nil {
		return fmt.Errorf("oidc/jwks: decode: %w", err)
	}
	keys := make(map[string]crypto.PublicKey, len(set.Keys))
	for _, k := range set.Keys {
		pub, err := parseJWK(k)
		if err != nil {
			continue // skip keys we can't parse (unknown kty/curve) rather than fail the set
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return fmt.Errorf("oidc/jwks: no usable keys")
	}
	c.byKID = keys
	c.fetchedAt = time.Now()
	return nil
}

// parseJWK reconstructs an RSA or EC public key from a JWK.
func parseJWK(k jwk) (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		nb, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, fmt.Errorf("rsa n: %w", err)
		}
		eb, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, fmt.Errorf("rsa e: %w", err)
		}
		// Exponent is a big-endian integer (commonly AQAB = 65537). Left-pad
		// to 8 bytes for binary.BigEndian.Uint64.
		if len(eb) == 0 || len(eb) > 8 {
			return nil, fmt.Errorf("rsa e size")
		}
		padded := make([]byte, 8)
		copy(padded[8-len(eb):], eb)
		e := binary.BigEndian.Uint64(padded)
		if e < 2 {
			return nil, fmt.Errorf("rsa e too small")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(e)}, nil
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, fmt.Errorf("ec curve %q", k.Crv)
		}
		xb, err := base64.RawURLEncoding.DecodeString(k.X)
		if err != nil {
			return nil, fmt.Errorf("ec x: %w", err)
		}
		yb, err := base64.RawURLEncoding.DecodeString(k.Y)
		if err != nil {
			return nil, fmt.Errorf("ec y: %w", err)
		}
		return &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(xb), Y: new(big.Int).SetBytes(yb)}, nil
	default:
		return nil, fmt.Errorf("unsupported kty %q", k.Kty)
	}
}

// verifyIDToken verifies the ID token's signature against the JWKS, and the
// iss / aud / exp standard claims (RFC 9700 §4.1), returning the parsed claims.
// The signing algorithm is restricted to the asymmetric allowlist to defeat
// alg-confusion (none / HMAC-with-public-key).
func verifyIDToken(ctx context.Context, cache *jwksCache, idToken, issuer, audience string) (idTokenClaims, error) {
	var claims idTokenClaims
	keyfunc := func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return cache.keyForKID(ctx, kid)
	}
	_, err := jwt.ParseWithClaims(idToken, &claims, keyfunc,
		jwt.WithValidMethods(jwksVerifyAlgs),
		jwt.WithIssuer(issuer),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return idTokenClaims{}, fmt.Errorf("oidc/jwks: verify id token: %w", err)
	}
	if claims.Subject == "" {
		return idTokenClaims{}, fmt.Errorf("oidc/jwks: id token missing sub")
	}
	return claims, nil
}
