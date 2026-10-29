package cli

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A cluster-wide baseline is a deliberate cross-tenant read: without a
// tenant, scoped must hand the store a team-blind context — the explicit
// bypass set, any caller stamp cleared — or the tenant filter's fail-closed
// guard panics on the first ListRuns (main's mongo-conformance on
// d51ce108e), and a caller stamped with one tenant would get that tenant's
// runs for "every tenant". The Mongo test
// (pkg/store/mongo TestReliabilityReport_BaselineOverAMongoStore) stays the
// integration witness; this one pins the contract without a broker.
func TestMongoReliabilitySourceScopesEveryTenantAsATeamBlindRead(t *testing.T) {
	stamped := store.WithIdentity(context.Background(), "tenant-z", "someone")
	ctx := mongoReliabilitySource{}.scoped(stamped)
	if !store.IsWithoutTenantFilter(ctx) {
		t.Fatal("no tenant configured: scoped carries neither a tenant nor the explicit bypass — the store's fail-closed guard panics on ListRuns")
	}
	if id, ok := store.TenantFromContext(ctx); ok || id != "" {
		t.Fatalf("no tenant configured, yet the caller's stamp %q survives: the cluster-wide baseline would be scoped back down to one tenant", id)
	}
}

// With a tenant, scoped carries that tenant and no bypass: a scoped listing
// whose loads were unscoped would pair one tenant's ids with another
// tenant's documents.
func TestMongoReliabilitySourceScopesATenantWithoutBypass(t *testing.T) {
	ctx := mongoReliabilitySource{tenant: "tenant-a"}.scoped(context.Background())
	if id, ok := store.TenantFromContext(ctx); !ok || id != "tenant-a" {
		t.Fatalf("tenant = %q (present=%v), want tenant-a on the context", id, ok)
	}
	if store.IsWithoutTenantFilter(ctx) {
		t.Fatal("a tenant-scoped source must not carry the cross-tenant bypass")
	}
}
