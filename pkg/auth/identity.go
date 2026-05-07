package auth

import (
	"context"

	"github.com/SocialGouv/iterion/pkg/identity"
)

// Identity is the authenticated principal extracted from the access
// JWT. Middleware injects it into the request ctx; handlers retrieve
// it via FromContext.
type Identity struct {
	UserID       string
	Email        string
	TeamID       string
	Role         identity.Role
	IsSuperAdmin bool
	// JTI is the JWT ID; useful for audit logging and explicit
	// revocation later (we don't revoke access tokens today; we
	// rely on short TTL + refresh rotation).
	JTI string
}

// HasRole reports whether the principal has at least the requested
// role *in their active team*. Super-admins always pass.
func (i Identity) HasRole(want identity.Role) bool {
	if i.IsSuperAdmin {
		return true
	}
	return i.Role.AtLeast(want)
}

type identityCtxKey struct{}

// WithIdentity returns a child ctx carrying the given Identity.
// Used by middleware after JWT validation.
func WithIdentity(parent context.Context, id Identity) context.Context {
	return context.WithValue(parent, identityCtxKey{}, id)
}

// FromContext returns the Identity carried by ctx and a boolean
// reporting whether one was set.
func FromContext(ctx context.Context) (Identity, bool) {
	if ctx == nil {
		return Identity{}, false
	}
	id, ok := ctx.Value(identityCtxKey{}).(Identity)
	return id, ok
}

// MustFromContext is a convenience for handlers that require an
// authenticated principal; it panics if none is present (which
// indicates a routing mistake — RequireAuth must wrap the handler).
func MustFromContext(ctx context.Context) Identity {
	id, ok := FromContext(ctx)
	if !ok {
		panic("auth: no Identity in context (RequireAuth missing?)")
	}
	return id
}
