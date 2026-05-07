package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// jwtIssuer is the value emitted in the iss claim. Constant so a
// client can reject tokens that didn't come from this server.
const jwtIssuer = "iterion"

// jwtAudience scopes tokens to the editor SPA + API. We don't issue
// audience-specific tokens today (the same JWT works for both), but
// pinning the value lets us split later without breaking existing
// clients on day-one.
const jwtAudience = "iterion-api"

// AccessClaims is the body of the access JWT. Embeds RegisteredClaims
// so iat/exp/jti/iss/aud/sub round-trip via golang-jwt.
type AccessClaims struct {
	Email        string `json:"email,omitempty"`
	TeamID       string `json:"team_id,omitempty"`
	Role         string `json:"role,omitempty"`
	IsSuperAdmin bool   `json:"is_super_admin,omitempty"`
	jwt.RegisteredClaims
}

// JWTSigner mints and verifies access JWTs.
type JWTSigner struct {
	secret    []byte
	accessTTL time.Duration
	now       func() time.Time // injected for tests
}

// NewJWTSigner constructs a signer from a base64-encoded HS256
// secret (>=32 bytes after decoding). The same string is used for
// signing and verifying; rotation strategy (TODO) will introduce a
// kid-keyed map.
func NewJWTSigner(b64 string, accessTTL time.Duration) (*JWTSigner, error) {
	key, err := decodeBase64Lenient(b64)
	if err != nil {
		return nil, fmt.Errorf("auth: decode jwt secret: %w", err)
	}
	if len(key) < 32 {
		return nil, fmt.Errorf("auth: jwt secret too short (%d bytes, need >=32)", len(key))
	}
	if accessTTL <= 0 {
		accessTTL = 15 * time.Minute
	}
	return &JWTSigner{secret: key, accessTTL: accessTTL, now: time.Now}, nil
}

// IssueAccess produces a freshly-signed access token for the given
// principal. The JTI is a UUIDv4 captured back into the returned
// Identity for audit purposes.
func (s *JWTSigner) IssueAccess(id Identity) (token string, exp time.Time, err error) {
	now := s.now().UTC()
	exp = now.Add(s.accessTTL)
	jti := uuid.NewString()
	claims := AccessClaims{
		Email:        id.Email,
		TeamID:       id.TeamID,
		Role:         string(id.Role),
		IsSuperAdmin: id.IsSuperAdmin,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    jwtIssuer,
			Subject:   id.UserID,
			Audience:  jwt.ClaimStrings{jwtAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        jti,
		},
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := t.SignedString(s.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: sign jwt: %w", err)
	}
	return signed, exp, nil
}

// Verify parses + validates a signed JWT, returning the Identity it
// carries. Errors are returned as a small set of categorized values
// so the middleware can map them to specific HTTP responses.
func (s *JWTSigner) Verify(raw string) (Identity, error) {
	parsed, err := jwt.ParseWithClaims(raw, &AccessClaims{}, func(t *jwt.Token) (interface{}, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.secret, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(jwtIssuer),
		jwt.WithAudience(jwtAudience),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		switch {
		case errors.Is(err, jwt.ErrTokenExpired):
			return Identity{}, ErrTokenExpired
		case errors.Is(err, jwt.ErrTokenNotValidYet),
			errors.Is(err, jwt.ErrTokenSignatureInvalid),
			errors.Is(err, jwt.ErrTokenMalformed),
			errors.Is(err, jwt.ErrTokenInvalidIssuer),
			errors.Is(err, jwt.ErrTokenInvalidAudience):
			return Identity{}, ErrTokenInvalid
		default:
			return Identity{}, fmt.Errorf("auth: verify jwt: %w", err)
		}
	}
	if !parsed.Valid {
		return Identity{}, ErrTokenInvalid
	}
	c, ok := parsed.Claims.(*AccessClaims)
	if !ok {
		return Identity{}, ErrTokenInvalid
	}
	return Identity{
		UserID:       c.Subject,
		Email:        c.Email,
		TeamID:       c.TeamID,
		Role:         identity.Role(c.Role),
		IsSuperAdmin: c.IsSuperAdmin,
		JTI:          c.ID,
	}, nil
}

// AccessTTL is exposed so callers (auth_routes) can stamp the cookie
// max-age in lock-step with the access expiry.
func (s *JWTSigner) AccessTTL() time.Duration { return s.accessTTL }

// decodeBase64Lenient mirrors secrets.decodeBase64Lenient — a copy
// to avoid an inter-package dep on an internal helper.
func decodeBase64Lenient(b64 string) ([]byte, error) {
	return secrets.DecodeBase64Lenient(b64)
}

// JWT-related sentinel errors.
var (
	ErrTokenExpired = errors.New("auth: token expired")
	ErrTokenInvalid = errors.New("auth: token invalid")
)
