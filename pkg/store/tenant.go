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

// withoutTenantFilterKey lets cluster-level admin paths (migration
// tools, queue dispatcher bootstrap, runview's reconcileOrphans,
// conformance tests) opt back into a tenant-less query. The mongo
// store treats a missing tenant as fail-closed (panic) unless this
// flag is set — every business-logic call site already runs under a
// tenant-stamped ctx, so a missing tenant_id is normally a bug.
type withoutTenantFilterKey struct{}

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

// WithoutTenantFilter marks ctx as exempt from the mongo store's
// tenant-scoped query guard. Use sparingly — every callsite is a
// potential tenant-isolation hole. Audit by grepping for callers.
func WithoutTenantFilter(parent context.Context) context.Context {
	return context.WithValue(parent, withoutTenantFilterKey{}, true)
}

// TeamBlind returns a context whose query sees the runs of every team:
// the caller's tenant stamp is cleared and the tenant-filter guard is
// lifted. A stamped tenant would scope the query right back down, so
// clearing it is half of the job. Every callsite is a potential
// tenant-isolation hole — audit by grepping for callers.
func TeamBlind(parent context.Context) context.Context {
	return WithoutTenantFilter(context.WithValue(parent, tenantCtxKey{}, ""))
}

// IsWithoutTenantFilter reports whether the ctx was tagged by
// WithoutTenantFilter. Read by mongo's withTenantFilter guard.
func IsWithoutTenantFilter(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(withoutTenantFilterKey{}).(bool)
	return v
}
