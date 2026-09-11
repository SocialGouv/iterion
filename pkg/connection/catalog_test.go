package connection_test

import (
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
)

// TestTheCatalogServesTheEFFECTIVEPackage — generated half plus overlay, which
// is what a launch must see.
//
// The catalog called `spec.Load`, which reads ops/ and knows nothing about
// overlays. So execution was served the GENERATED package while `iterion
// connectors validate` reported on the merged one, and the two disagreed about
// everything the authored half exists to say. The failure was invisible in both
// directions: validation was green, and a run failed with "no operation" on an
// id the ADR advertises.
//
// It runs against the SHIPPED Forgejo package rather than a fixture, because a
// fixture with no overlay — which is what the end-to-end test uses — cannot
// distinguish the two loaders at all.
func TestTheCatalogServesTheEffectivePackage(t *testing.T) {
	root := filepath.Join("..", "..", "connectors")
	if _, err := connection.NewFSCatalog(root).Package("forgejo"); err != nil {
		t.Skipf("the shipped Forgejo package is not present: %v", err)
	}
	pkg, err := connection.NewFSCatalog(root).Package("forgejo")
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	// The overlay PINS this id. Without the overlay the operation answers to
	// its derived name (`create_comment`) and a `.bot` writing the documented
	// one fails at resolution.
	if _, ok := pkg.Operation("forgejo.issue.comment"); !ok {
		t.Error("the overlay's pinned operation id is absent — the authored half was not applied")
	}

	// The overlay REPLACES auth: the description declares five schemes, three
	// of which are not credentials at all (an admin-impersonation modifier and
	// a second factor). Serving those would ask an operator to authenticate
	// with something that is not an identity.
	if n := len(pkg.Connector.Auth); n != 2 {
		ids := make([]string, 0, n)
		for _, a := range pkg.Connector.Auth {
			ids = append(ids, a.ID)
		}
		t.Errorf("auth schemes = %v, want the overlay's two", ids)
	}

	// And the pagination the overlay declares must be there, or a walk of a
	// repository's issues returns one page and calls it the collection.
	op, ok := pkg.Operation("forgejo.issue.list")
	if !ok {
		t.Fatal("forgejo.issue.list is absent")
	}
	if op.Pagination == nil {
		t.Error("the overlay's pagination was not applied — a walk would return one page")
	}
}
