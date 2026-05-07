package mongo

import (
	"context"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/SocialGouv/iterion/pkg/store"
)

// withTenantFilter augments a Mongo filter with a tenant_id clause
// when the ctx carries one. Privileged callers (no tenant in ctx)
// see the unmodified filter — cluster-level admin tools, the runner
// during its bootstrap before the tenant has been verified, and the
// migration tooling all rely on this escape hatch.
func withTenantFilter(ctx context.Context, base bson.M) bson.M {
	tenantID, ok := store.TenantFromContext(ctx)
	if !ok {
		return base
	}
	out := make(bson.M, len(base)+1)
	for k, v := range base {
		out[k] = v
	}
	out["tenant_id"] = tenantID
	return out
}

// stampTenant copies the tenant_id from ctx onto a Run before
// persisting it. Idempotent: an already-set tenant_id is preserved.
func stampTenant(ctx context.Context, r *store.Run) {
	if r == nil {
		return
	}
	if r.TenantID != "" {
		return
	}
	if id, ok := store.TenantFromContext(ctx); ok {
		r.TenantID = id
	}
	if r.OwnerID == "" {
		if uid, ok := store.OwnerFromContext(ctx); ok {
			r.OwnerID = uid
		}
	}
}

// stampTenantOnEvent does the same for Event documents.
func stampTenantOnEvent(ctx context.Context, e *store.Event) {
	if e == nil || e.TenantID != "" {
		return
	}
	if id, ok := store.TenantFromContext(ctx); ok {
		e.TenantID = id
	}
}
