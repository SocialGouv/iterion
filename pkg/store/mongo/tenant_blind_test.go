package mongo

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A team-blind context must reach withTenantFilter as a query on every
// team: the caller's stamp cleared (a stamped tenant scopes the query back
// down, silently re-narrowing the lookup) and the fail-closed guard
// lifted. Either half missing is pinned here.
func TestWithTenantFilter_TeamBlindQueriesEveryTeam(t *testing.T) {
	stamped := store.WithTenant(context.Background(), "tenant-A")
	if _, ok := store.TenantFromContext(stamped); !ok {
		t.Fatal("setup: the stamped context carries no tenant")
	}

	blind := store.TeamBlind(stamped)
	if _, ok := store.TenantFromContext(blind); ok {
		t.Fatal("TeamBlind left the caller's tenant stamp on the context")
	}
	got := withTenantFilter(blind, map[string]any{"_id": "run-1"})
	if _, has := got["tenant_id"]; has {
		t.Fatalf("withTenantFilter(team-blind) = %v, want no tenant filter", got)
	}

	// The stamp still scopes a query that is not team-blind, and a bare
	// unstamped context without the filter still panics — the fail-closed
	// guard is the backstop for the lookups that claim to be team-blind.
	if f := withTenantFilter(stamped, map[string]any{"_id": "run-1"}); f["tenant_id"] != "tenant-A" {
		t.Fatalf("withTenantFilter(stamped) = %v, want the caller's tenant", f)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("withTenantFilter on an unstamped, unfiltered context did not panic")
			}
		}()
		_ = withTenantFilter(context.Background(), map[string]any{"_id": "run-1"})
	}()
}
