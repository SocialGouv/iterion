package server

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
)

// The facade's per-grant composition (#2757): with the package store wired,
// a tenant's catalog walks team → platform → baked; without it the server
// answers nil, and the facade keeps serving the static catalog.
func TestCatalogForTenant_ComposesTheThreeTiers(t *testing.T) {
	s := &Server{}
	if got := s.catalogForTenant("t1"); got != nil {
		t.Fatalf("no package store wired: got %v, want nil (the static catalog serves)", got)
	}

	store := connection.NewMemoryPackageStore()
	s.connectorPackages = store
	cat := s.catalogForTenant("t1")
	if cat == nil {
		t.Fatal("a wired store must compose the tiers")
	}
	// The floor answers through the composition: forgejo is baked, and the
	// answer names that tier.
	if _, tier, err := cat.Package("forgejo"); err != nil || tier.Kind != connection.TierBaked {
		t.Fatalf("baked through the composition: tier=%v err=%v", tier.Kind, err)
	}
}
