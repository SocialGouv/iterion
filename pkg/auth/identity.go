package auth

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// Identity is the authenticated principal extracted from the access
// JWT. Middleware injects it into the request ctx; handlers retrieve
// it via FromContext.
type Identity struct {
	UserID string
	Email  string
	// OrgID / OrgRole are the active organization and the principal's
	// role within it. A token minted before the org rollout carries an
	// empty OrgID; gates that need an org derive it from the active
	// team's OrgID and the session self-heals on the next refresh.
	OrgID        string
	OrgRole      identity.OrgRole
	TeamID       string
	Role         identity.Role
	IsSuperAdmin bool
	// Kind distinguishes a real authenticated user from a synthetic,
	// purpose-scoped principal minted by a self-authenticating surface
	// (see IdentityKind). The zero value is a real user.
	Kind IdentityKind
	// JTI is the JWT ID; useful for audit logging and explicit
	// revocation later (we don't revoke access tokens today; we
	// rely on short TTL + refresh rotation).
	JTI string
}

// IdentityKind distinguishes a real authenticated user (JWT or PAT — full
// role in the active team) from a synthetic, purpose-scoped principal minted
// by a self-authenticating surface: an inbound webhook, or a config-share
// editor link. A synthetic identity carries a TeamID + Role so tenant-scoped
// STORE reads still work, but it must NEVER pass the operator RBAC gates
// (canViewTeam / canManageTeam / …) — those authorize humans acting on a
// team's resources. The zero value ("") is a real user, so existing code
// that builds an Identity without a Kind keeps full behaviour; only the two
// synthetic authenticators set a non-empty Kind.
type IdentityKind string

const (
	KindUser    IdentityKind = ""        // JWT or PAT — a real user (zero value)
	KindWebhook IdentityKind = "webhook" // inbound-webhook launch trigger
	KindShare   IdentityKind = "share"   // config-share editor link
)

// IsSynthetic reports whether this principal is a purpose-scoped,
// self-authenticating identity (webhook/share) rather than a real user.
// Operator RBAC gates deny synthetic identities by default; an endpoint that
// intentionally serves one opts in explicitly.
func (i Identity) IsSynthetic() bool {
	return i.Kind == KindWebhook || i.Kind == KindShare
}

// HasRole reports whether the principal has at least the requested
// role *in their active team*. Super-admins always pass.
func (i Identity) HasRole(want identity.Role) bool {
	if i.IsSuperAdmin {
		return true
	}
	return i.Role.AtLeast(want)
}

// HasOrgRole reports whether the principal has at least the requested
// role *in their active org*. Super-admins always pass.
func (i Identity) HasOrgRole(want identity.OrgRole) bool {
	if i.IsSuperAdmin {
		return true
	}
	return i.OrgRole.AtLeast(want)
}

type identityCtxKey struct{}

// WithIdentity returns a child ctx carrying the given Identity.
// Used by middleware after JWT validation.
func WithIdentity(parent context.Context, id Identity) context.Context {
	return context.WithValue(parent, identityCtxKey{}, id)
}

// FromContext returns the Identity carried by ctx and a boolean
// reporting whether one was set. Handlers that need authentication
// should check the second return and surface a 401/500 to the caller
// — never panic. Middleware (RequireAuth) is the right place to gate
// admission, not a panic in the handler body.
func FromContext(ctx context.Context) (Identity, bool) {
	if ctx == nil {
		return Identity{}, false
	}
	id, ok := ctx.Value(identityCtxKey{}).(Identity)
	return id, ok
}
