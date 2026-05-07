package store

import "context"

// tenantCtxKey is the unexported key under which auth-aware callers
// stash the active tenant_id in the context. The Mongo store reads
// it via TenantFromContext to scope every query — never trust a
// caller that bypasses the helper.
type tenantCtxKey struct{}

// ownerCtxKey carries the user_id of the principal who initiated the
// request. Stamped into newly-created Run / Event documents so admin
// surfaces can attribute work without consulting an audit log.
type ownerCtxKey struct{}

// WithTenant returns a child context carrying the given tenant_id.
// An empty tenantID returns parent unchanged — the caller is then
// responsible for ensuring the request is permitted to bypass tenant
// scoping (super-admin paths, runner pickup before the tenant has
// been verified, etc.).
func WithTenant(parent context.Context, tenantID string) context.Context {
	if tenantID == "" {
		return parent
	}
	return context.WithValue(parent, tenantCtxKey{}, tenantID)
}

// WithOwner returns a child context carrying the initiating user_id.
func WithOwner(parent context.Context, userID string) context.Context {
	if userID == "" {
		return parent
	}
	return context.WithValue(parent, ownerCtxKey{}, userID)
}

// WithIdentity is a convenience that stamps both tenant_id and user_id
// in one call. Used by the server's auth middleware after JWT decode.
func WithIdentity(parent context.Context, tenantID, userID string) context.Context {
	return WithOwner(WithTenant(parent, tenantID), userID)
}

// TenantFromContext returns the tenant_id stamped on ctx and a flag
// indicating whether one was set. Mongo queries should always use
// the (id, ok) pair: when ok is false the call is implicitly
// privileged (runner bootstrap, migration tooling, super-admin).
func TenantFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	v, ok := ctx.Value(tenantCtxKey{}).(string)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// OwnerFromContext mirrors TenantFromContext for the user_id slot.
func OwnerFromContext(ctx context.Context) (string, bool) {
	if ctx == nil {
		return "", false
	}
	v, ok := ctx.Value(ownerCtxKey{}).(string)
	if !ok || v == "" {
		return "", false
	}
	return v, true
}
