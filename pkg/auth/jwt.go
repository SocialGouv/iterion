package auth

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/SocialGouv/iterion/pkg/identity"
	"github.com/SocialGouv/iterion/pkg/secrets"
)

// jwtIssuer is the value emitted in the iss claim. Constant so a
// client can reject tokens that didn't come from this server.
const jwtIssuer = "iterion"

// jwtAudience scopes tokens to the studio SPA + API. We don't issue
// audience-specific tokens today (the same JWT works for both), but
// pinning the value lets us split later without breaking existing
// clients on day-one.
const jwtAudience = "iterion-api"

// AccessClaims is the body of the access JWT. Embeds RegisteredClaims
// so iat/exp/jti/iss/aud/sub round-trip via golang-jwt.
type AccessClaims struct {
	Email        string `json:"email,omitempty"`
	OrgID        string `json:"org_id,omitempty"`
	OrgRole      string `json:"org_role,omitempty"`
	TeamID       string `json:"team_id,omitempty"`
	Role         string `json:"role,omitempty"`
	IsSuperAdmin bool   `json:"is_super_admin,omitempty"`
	jwt.RegisteredClaims
}

// JWTKey is a single HS256 signing key, identified by a stable kid so
// tokens minted under it can be verified after the active key has
// rotated. kid is stamped into the JWT header at sign time and looked
// up out of the header at verify time.
type JWTKey struct {
	ID     string
	Secret []byte
}

// JWTDenylist is the abstraction the signer uses to look up revoked
// JTIs. Implementations are expected to be backed by storage with a
// TTL index at the JWT's exp so the list doesn't grow unbounded.
// Verify checks IsDenied for every successful parse — an empty
// implementation (no revocations) is provided as NopDenylist.
type JWTDenylist interface {
	IsDenied(jti string) bool
}

// NopDenylist accepts every JTI. The signer falls back to it when no
// explicit denylist is supplied so test setups don't need to wire
// storage.
type NopDenylist struct{}

func (NopDenylist) IsDenied(string) bool { return false }

// JWTSigner mints and verifies access JWTs across multiple signing
// keys. The active key (keys[activeKID]) is used for IssueAccess;
// Verify accepts a token signed by any key in the map, dispatching by
// the `kid` header. A token that arrives without a kid header is
// assumed to come from the active key — that lets the first rollout
// land without breaking already-issued tokens.
type JWTSigner struct {
	keys      map[string]JWTKey
	activeKID string
	accessTTL time.Duration
	denylist  JWTDenylist
	now       func() time.Time // injected for tests
}

// NewJWTSigner constructs a signer from a single key (the simple
// single-secret case). Equivalent to NewJWTSignerMulti with one entry
// in the map and the same kid marked active. Kept for callers that
// don't need rotation; new deployments should prefer NewJWTSignerMulti.
func NewJWTSigner(b64 string, accessTTL time.Duration) (*JWTSigner, error) {
	key, err := secrets.DecodeBase64Lenient(b64)
	if err != nil {
		return nil, fmt.Errorf("auth: decode jwt secret: %w", err)
	}
	if len(key) < 32 {
		return nil, fmt.Errorf("auth: jwt secret too short (%d bytes, need >=32)", len(key))
	}
	if accessTTL <= 0 {
		accessTTL = 15 * time.Minute
	}
	return &JWTSigner{
		keys:      map[string]JWTKey{"k0": {ID: "k0", Secret: key}},
		activeKID: "k0",
		accessTTL: accessTTL,
		denylist:  NopDenylist{},
		now:       time.Now,
	}, nil
}

// NewJWTSignerMulti constructs a signer that signs new tokens with
// keys[activeKID] and verifies tokens against any key in keys. The
// caller supplies kids; recommended scheme is monotonically increasing
// "k0", "k1", "k2" so the active key is obvious in metrics and logs.
// During a rollover, retire a key by removing it from the map after
// one AccessTTL has elapsed past the last token it signed.
func NewJWTSignerMulti(keys []JWTKey, activeKID string, accessTTL time.Duration, denylist JWTDenylist) (*JWTSigner, error) {
	if len(keys) == 0 {
		return nil, errors.New("auth: at least one JWT key required")
	}
	m := make(map[string]JWTKey, len(keys))
	for _, k := range keys {
		if k.ID == "" {
			return nil, errors.New("auth: JWT key requires a non-empty ID")
		}
		if len(k.Secret) < 32 {
			return nil, fmt.Errorf("auth: JWT key %q too short (%d bytes, need >=32)", k.ID, len(k.Secret))
		}
		if _, dup := m[k.ID]; dup {
			return nil, fmt.Errorf("auth: duplicate JWT key id %q", k.ID)
		}
		m[k.ID] = k
	}
	if activeKID == "" {
		// Default to the lexicographically-largest kid — operationally
		// the convention is monotonically increasing names ("k0" → "k1").
		ids := make([]string, 0, len(m))
		for id := range m {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		activeKID = ids[len(ids)-1]
	}
	if _, ok := m[activeKID]; !ok {
		return nil, fmt.Errorf("auth: active JWT kid %q not in key set", activeKID)
	}
	if accessTTL <= 0 {
		accessTTL = 15 * time.Minute
	}
	if denylist == nil {
		denylist = NopDenylist{}
	}
	return &JWTSigner{
		keys:      m,
		activeKID: activeKID,
		accessTTL: accessTTL,
		denylist:  denylist,
		now:       time.Now,
	}, nil
}

// IssueAccess produces a freshly-signed access token for the given
// principal. The JTI is a UUIDv4 captured back into the returned
// Identity for audit purposes; the kid header is the signer's active
// key id so Verify can dispatch correctly after rotation.
func (s *JWTSigner) IssueAccess(id Identity) (token string, exp time.Time, err error) {
	now := s.now().UTC()
	exp = now.Add(s.accessTTL)
	jti := uuid.NewString()
	claims := AccessClaims{
		Email:        id.Email,
		OrgID:        id.OrgID,
		OrgRole:      string(id.OrgRole),
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
	t.Header["kid"] = s.activeKID
	signed, err := t.SignedString(s.keys[s.activeKID].Secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: sign jwt: %w", err)
	}
	return signed, exp, nil
}

// Verify parses + validates a signed JWT, returning the Identity it
// carries. Errors are returned as a small set of categorized values
// so the middleware can map them to specific HTTP responses.
func (s *JWTSigner) Verify(raw string) (Identity, error) {
	parsed, err := jwt.ParseWithClaims(raw, &AccessClaims{}, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		// Look the key up by kid. Absent header → fall back to the
		// active key so tokens that predate the rotation work-around
		// keep verifying until they expire.
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			kid = s.activeKID
		}
		key, ok := s.keys[kid]
		if !ok {
			return nil, fmt.Errorf("unknown kid %q", kid)
		}
		return key.Secret, nil
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
	if c.ID != "" && s.denylist.IsDenied(c.ID) {
		return Identity{}, ErrTokenRevoked
	}
	return Identity{
		UserID:       c.Subject,
		Email:        c.Email,
		OrgID:        c.OrgID,
		OrgRole:      identity.OrgRole(c.OrgRole),
		TeamID:       c.TeamID,
		Role:         identity.Role(c.Role),
		IsSuperAdmin: c.IsSuperAdmin,
		JTI:          c.ID,
	}, nil
}

// AccessTTL is exposed so callers (auth_routes) can stamp the cookie
// max-age in lock-step with the access expiry.
func (s *JWTSigner) AccessTTL() time.Duration { return s.accessTTL }

// ActiveKID surfaces the active signing key id so operators can verify
// (via /authz/diagnostics, for example) which key the server is
// currently minting tokens with.
func (s *JWTSigner) ActiveKID() string { return s.activeKID }

// JWT-related sentinel errors.
var (
	ErrTokenExpired = errors.New("auth: token expired")
	ErrTokenInvalid = errors.New("auth: token invalid")
	ErrTokenRevoked = errors.New("auth: token revoked")
)
