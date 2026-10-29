package connection_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/connection"
)

// TestTheBakedFloorServesTheShippedPackageWithNoHomeAndNoWorkspace.
//
// A connector package is a directory of YAML, so until it travelled in the
// binary the only processes that could resolve one were those standing in a
// checkout of this repository: a published `iterion` served NO connector, and
// a `.bot` calling `forgejo.issue.comment` failed with "no connector catalog"
// on every machine that installed the binary instead of cloning the tree.
//
// The process here has neither a home nor a workspace to read: the working
// directory is an empty one and both home variables point at empty
// directories, so an answer can only have come from the binary.
func TestTheBakedFloorServesTheShippedPackageWithNoHomeAndNoWorkspace(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ITERION_HOME", t.TempDir())

	pkg, tier, err := connection.BakedCatalog().Package("forgejo")
	if err != nil {
		t.Fatalf("the baked floor must serve the shipped package: %v", err)
	}
	if tier.Kind != connection.TierBaked {
		t.Errorf("tier = %+v, want the baked kind", tier)
	}
	// Source stays empty and the phrase says so: a tier that lives in the
	// binary must not print a directory somebody would go looking in.
	if tier.Source != "" || tier.String() != "baked catalog" {
		t.Errorf("tier = %+v (%q), want no source and the phrase \"baked catalog\"", tier, tier.String())
	}

	// The EFFECTIVE package — the overlay travelled with the generated half.
	// Without it the operation answers to its derived name (`create_comment`)
	// and the connector runs with the five auth schemes the vendor's
	// description declares instead of the two iterion states.
	if _, ok := pkg.Operation("forgejo.issue.comment"); !ok {
		t.Error("the overlay's pinned operation id is absent — the baked copy carries the generated half only")
	}
	if n := len(pkg.Connector.Auth); n != 2 {
		t.Errorf("auth schemes = %d, want the overlay's two", n)
	}

	// And it ADVERTISES what it holds, which is what a near-miss diagnostic
	// and the studio read.
	if got := connection.BakedCatalog().Connectors(); len(got) != 1 || got[0] != "forgejo" {
		t.Errorf("Connectors() = %v, want the shipped package", got)
	}
}

// TestTheBakedFloorIsNotWiredIntoTheLocalTiersYet.
//
// S1 of a sequenced wave lands the floor BUILDABLE and unwired (ARB-68): a
// tier that always answers makes `LocalCatalogs` return at least one tier
// forever, so `LocalResolver` never returns nil and the refusal "this process
// has no connector catalog wired" becomes unreachable — a load-bearing
// refusal, with a greppable override, that keeps a `.bot` from spending an
// operator's credential on a resolution nobody configured. Composing the
// floor and re-aiming those surfaces is ONE change; this test is what says
// the wiring has not happened yet, so the day it does, it is deliberate.
func TestTheBakedFloorIsNotWiredIntoTheLocalTiersYet(t *testing.T) {
	tiers, err := connection.LocalCatalogs(connection.LocalPaths{}, trustedRun())
	if err != nil {
		t.Fatalf("no roots at all is not an error: %v", err)
	}
	if len(tiers) != 0 {
		t.Errorf("tiers = %d with no roots, want none: the local tiers must not serve the baked floor yet", len(tiers))
	}

	// With a home root that holds nothing, the shipped connector is still not
	// resolvable through the local tiers — the floor is reachable only by
	// name.
	local, err := connection.LocalCatalogs(connection.LocalPaths{Home: t.TempDir()}, trustedRun())
	if err != nil {
		t.Fatalf("an empty home root is a tier, not an error: %v", err)
	}
	if _, _, err := connection.NewLayeredCatalog(local...).Package("forgejo"); err == nil {
		t.Error("the local tiers served the shipped connector — the baked floor is wired, which this slice must not do")
	}

	// The falsifier: the floor itself does hold it, so the assertion above is
	// about the WIRING and not about an empty binary.
	if _, _, err := connection.BakedCatalog().Package("forgejo"); err != nil {
		t.Errorf("the floor must hold the shipped connector: %v", err)
	}
}

// TestAConnectorTheBakedFloorDoesNotHoldFallsThrough.
//
// The floor is the LAST tier, so its "not here" has to carry the one
// sentinel a layered lookup may fall through on. A floor that reported an
// ordinary error would stop the search at itself and shadow every tier
// composed after it.
func TestAConnectorTheBakedFloorDoesNotHoldFallsThrough(t *testing.T) {
	stored := connection.NewMemoryCatalog(tierPackage("https://probe.example", "POST", "/issues"))
	// The floor FIRST, so the fall-through is the only way the memory tier
	// can be reached.
	layered := connection.NewLayeredCatalog(connection.BakedCatalog(), stored)
	_, tier, err := layered.Package("probe")
	if err != nil {
		t.Fatalf("a connector the floor does not hold must fall through: %v", err)
	}
	if tier.Kind != connection.TierMemory {
		t.Errorf("tier = %+v, want the tier that actually holds the package", tier)
	}

	// And the refusal NAMES the floor, rather than an empty directory: the
	// one reader of this text is a person debugging a connector that did not
	// resolve.
	_, _, err = connection.BakedCatalog().Package("probe")
	if err == nil {
		t.Fatal("the floor does not hold probe")
	}
	for _, want := range []string{"baked catalog", "forgejo"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal = %v, want it to name %q", err, want)
		}
	}
}
