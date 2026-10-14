package bundle

import (
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/floortest"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// collapseTarget is the newest release the registry names OTHER than the one
// the newest profile's pin carries: collapsing that pin onto it puts two
// floors on one release, which is what the collapsed order is. The pin's own
// release would collapse nothing — the day a profile lot ships, that pin is
// the newest of all.
func collapseTarget(t *testing.T) string {
	t.Helper()
	pin := parser.ProfileSince[parser.MaxProfile]
	var top string
	for name, release := range SyntaxFloors() {
		if release == pin {
			continue
		}
		if top == "" {
			top = release
			continue
		}
		cmp, ok := CompareVersions(release, top)
		if !ok {
			t.Fatalf("%s = %q cannot be ordered", name, release)
		}
		if cmp > 0 {
			top = release
		}
	}
	if top == "" {
		t.Fatalf("every pin of the registry names %s, the newest profile's release: there is no second floor to collapse onto", pin)
	}
	return top
}

// needsTheHighestPinTouched holds what the floor predicate guarantees for a
// bundle that uses several syntaxes: the release asked for is the highest of
// the pins its sources touch, and the reason names a syntax pinned at that
// release. Which of two syntaxes pinned at ONE release answers is the table's
// order, not a promise — a release that cuts two floors at once makes them
// tie — so nothing here asserts a ranking of the pins.
func needsTheHighestPinTouched(t *testing.T, pf ProfileFloor, pins map[string]string) {
	t.Helper()
	var top string
	for reason, release := range pins {
		if top == "" {
			top = release
			continue
		}
		cmp, ok := CompareVersions(release, top)
		if !ok {
			t.Fatalf("the pin of %s, %q, cannot be ordered against %q", reason, release, top)
		}
		if cmp > 0 {
			top = release
		}
	}
	if pf.Need != top {
		t.Fatalf("the floor asks for %q, not %q, the highest release of %v: %+v", pf.Need, top, pins, pf)
	}
	if pins[pf.Reason] != pf.Need {
		t.Fatalf("the reason %q does not name a syntax of %v pinned at %q: %+v", pf.Reason, pins, pf.Need, pf)
	}
}

// A floor below the release a set of sources needs is refused, and the
// release itself admitted, however the pins are numbered — including the day
// two floors are cut in the same release, where the aligner writes that
// version into both and the pins stop being ordered by syntax.
//
// The predicate this walks is the one every floor surface reads: `validate`'s
// C252 (pkg/bundlelint), the admin push (pkg/server) and `dsl migrate`
// (pkg/cli) all come through CheckSyntaxFloor and RequiredRelease.
func TestEveryFloorRefusesAReleaseBelowItWhateverTheOtherPinsName(t *testing.T) {
	asked := map[string]string{}
	floortest.EachPinOrder(t, collapseTarget(t), func(t *testing.T) {
		var needs []string
		for _, req := range floorProbes() {
			need, reason := RequiredRelease(req)
			if need == "" {
				t.Errorf("%s asks for no release", req.Describe())
				continue
			}
			needs = append(needs, req.Describe()+" "+need)
			low := floortest.Below(t, need)
			if pf := CheckSyntaxFloor(&Manifest{Requires: &Requires{Iterion: ">= " + low}}, req); pf.OK {
				t.Errorf("%s: a floor of %s passed for %s, the release %s needs", req.Describe(), low, need, reason)
			}
			if pf := CheckSyntaxFloor(&Manifest{Requires: &Requires{Iterion: ">= " + need}}, req); !pf.OK {
				t.Errorf("%s: the floor %s itself was refused: %+v", req.Describe(), need, pf)
			}
		}
		asked[t.Name()] = strings.Join(needs, ", ")
	})
	// What the probes are asked for has to differ between the two orders:
	// a collapse that moved a pin no probe reads would leave the second
	// case a copy of the first, green and exercising nothing.
	if len(asked) != 2 {
		t.Fatalf("the probes ran in %d pin orders: %v", len(asked), asked)
	}
	var seen []string
	for _, needs := range asked {
		seen = append(seen, needs)
	}
	if seen[0] == seen[1] {
		t.Fatalf("both pin orders ask for the same releases (%s): the collapsed order reached no pin the probes read", seen[0])
	}
}

// A bundle that uses several syntaxes at once — the shape where the floor has
// to choose among the pins — asks for the highest release they name, in both
// orders. The pins each case touches are named here, not read back from the
// table the predicate itself walks.
func TestAMultiSyntaxBundleAsksForTheHighestPinItTouches(t *testing.T) {
	floortest.EachPinOrder(t, collapseTarget(t), func(t *testing.T) {
		profile := fmt.Sprintf("dsl profile %d", parser.MaxProfile)
		for _, probe := range []struct {
			req  SyntaxRequirements
			pins map[string]string
		}{
			{
				SyntaxRequirements{Profile: parser.MaxProfile, DeclaredBy: []string{"main.bot"}, ImportedBy: []string{"main.bot"}},
				map[string]string{profile: parser.ProfileSince[parser.MaxProfile], "import": parser.ImportSince},
			},
			{
				SyntaxRequirements{Profile: parser.MaxProfile, DeclaredBy: []string{"main.bot"}, ImportedBy: []string{"main.bot"}, ContractedBy: []string{"lib/c.bot"}},
				map[string]string{profile: parser.ProfileSince[parser.MaxProfile], "import": parser.ImportSince, "contract": parser.ContractSince},
			},
			{
				SyntaxRequirements{Profile: 1, ImportedBy: []string{"main.bot"}, AliasBy: []string{"main.bot"}, MatchingBy: []string{"main.bot"}, EmptyToolsBy: []string{"main.bot"}},
				map[string]string{
					"import":                                parser.ImportSince,
					"the Claw tool alias":                   ToolAliasesSince,
					`a var's [matching: "<re>"] constraint`: parser.VarMatchingSince,
					"an empty `tools: []` declaration":      DeclaredEmptyToolsSince,
				},
			},
		} {
			needsTheHighestPinTouched(t, CheckSyntaxFloor(nil, probe.req), probe.pins)
		}
	})
}
