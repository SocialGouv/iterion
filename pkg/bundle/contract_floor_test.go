package bundle

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/internal/floortest"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// floorProbes asks, one per entry of the syntax-floor table, for that
// entry's release: the list every walk of the table shares, so a floor added
// to the table is held by each of them at once.
func floorProbes() []SyntaxRequirements {
	return []SyntaxRequirements{
		{Profile: parser.MaxProfile, DeclaredBy: []string{"main.bot"}},
		{Profile: 1, ImportedBy: []string{"main.bot"}},
		{Profile: 1, ContractedBy: []string{"main.bot"}},
		{Profile: 1, AliasBy: []string{"main.bot"}},
		{Profile: 1, MatchingBy: []string{"main.bot"}},
		{Profile: 1, EmptyToolsBy: []string{"main.bot"}},
	}
}

// A bundle that declares a contract asks for the release that reads
// `contract` — the walk names the files that declare one, in the main or in
// a fragment, and the floor predicate takes the highest release among what
// the sources use; a bundle that uses nothing asks for nothing.
//
// The floor cases run in both pin orders: the day `contract` and the profile
// are cut in one release, a case that wrote "a release below the contract
// release" as another pin would be writing the contract release itself.
func TestSyntaxRequirementsRecordContractsAndAskForTheirRelease(t *testing.T) {
	files := map[string]string{
		"main.bot":  "import \"lib/c.bot\"\n\nworkflow w:\n  contract: c\n  entry: done\n",
		"lib/c.bot": "contract c:\n  version: 1\n",
	}
	req := MaxSyntaxRequirements(files)
	if !reflect.DeepEqual(req.ContractedBy, []string{"lib/c.bot"}) || !req.UsesContract() || !req.Asks() {
		t.Fatalf("requirements %+v", req)
	}
	profile := fmt.Sprintf("dsl profile %d", parser.MaxProfile)
	floortest.EachPinOrder(t, parser.ContractSince, func(t *testing.T) {
		one := SyntaxRequirements{Profile: 1, ContractedBy: []string{"main.bot"}}
		if pf := CheckSyntaxFloor(nil, one); pf.OK || pf.Need != parser.ContractSince || pf.Reason != "contract" {
			t.Fatalf("no manifest: %+v", pf)
		}
		// floortest:exempt ContractSince — the floor declared here is the one
		// this case is AT, read from its own pin; the release BELOW it comes
		// from floortest.Below, never from a sibling pin.
		if pf := CheckSyntaxFloor(&Manifest{Requires: &Requires{Iterion: ">= " + parser.ContractSince}}, one); !pf.OK {
			t.Fatalf("the contract release declared: %+v", pf)
		}
		if pf := CheckSyntaxFloor(&Manifest{Requires: &Requires{Iterion: ">= " + floortest.Below(t, parser.ContractSince)}}, one); pf.OK {
			t.Fatalf("a release below the contract release passed for a contract: %+v", pf)
		}
		if got := one.Describe(); got != "`contract` (main.bot)" {
			t.Fatalf("describe: %q", got)
		}
		// The newest profile is the one the collapse repoints, so the second
		// order really puts two floors on one release.
		all := SyntaxRequirements{Profile: parser.MaxProfile, DeclaredBy: []string{"main.bot"}, ImportedBy: []string{"main.bot"}, ContractedBy: []string{"lib/c.bot"}}
		needsTheHighestPinTouched(t, CheckSyntaxFloor(nil, all), map[string]string{
			profile:    parser.ProfileSince[parser.MaxProfile],
			"import":   parser.ImportSince,
			"contract": parser.ContractSince,
		})
		if want := profile + " (main.bot) and `import` (main.bot) and `contract` (lib/c.bot)"; all.Describe() != want {
			t.Fatalf("describe: %q, want %q", all.Describe(), want)
		}
	})
	for _, quiet := range []SyntaxRequirements{{}, {Profile: 1}, {Profile: 1, Unread: []string{"x"}}} {
		if quiet.Asks() {
			t.Fatalf("%+v asks for a floor", quiet)
		}
	}
	// The registry of floors names every pin, so a floor added later is
	// held by the release test and the scaffold alike. It reads the pins
	// outside any collapsed order: SyntaxFloors projects a table built at
	// package init, while the pin a collapse repoints is read live.
	floors := SyntaxFloors()
	profilePin := fmt.Sprintf("parser.ProfileSince[%d]", parser.MaxProfile)
	if floors["parser.ContractSince"] != parser.ContractSince || floors["parser.ImportSince"] != parser.ImportSince || floors[profilePin] != parser.ProfileSince[parser.MaxProfile] {
		t.Fatalf("SyntaxFloors: %v", floors)
	}
}

// Every release the table can ask for is a pin of the registry, and every
// entry contributes at least one pin: the two views of the table cannot
// drift, since both are projections of it.
func TestEveryFloorTheTableAsksForIsPinned(t *testing.T) {
	pinned := map[string]bool{}
	for _, release := range SyntaxFloors() {
		pinned[release] = true
	}
	probes := floorProbes()
	for i, f := range syntaxFloors {
		if len(f.pins) == 0 {
			t.Errorf("table entry %d contributes no pin to the registry", i)
		}
		var asked bool
		for _, req := range probes {
			release, reason, asks := f.need(req)
			if !asks {
				continue
			}
			asked = true
			if !pinned[release] {
				t.Errorf("table entry %d asks for %s (%s), which no pin of the registry names", i, release, reason)
			}
		}
		if !asked {
			t.Errorf("table entry %d asks for nothing on any probe: the probes are stale", i)
		}
	}
	for p := 2; p <= parser.MaxProfile; p++ {
		if !pinned[parser.ProfileSince[p]] {
			t.Errorf("profile %d's release is not in the registry", p)
		}
	}
}
