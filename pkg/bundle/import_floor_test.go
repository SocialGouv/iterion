package bundle

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/internal/floortest"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A bot in several files asks for the release that reads `import` — the
// walk names the files that import, and the floor predicate takes the
// highest release among what the sources use.
//
// The floor cases run in both pin orders: the day `import` and the profile
// are cut in one release, a case that wrote "a release below the import
// release" as the profile's pin would be writing the import release itself.
func TestSyntaxRequirementsRecordImportsAndAskForTheirRelease(t *testing.T) {
	files := map[string]string{
		"main.bot":          "import \"lib/nodes.bot\"\n\nsubbot child:\n  source: \"kids/k.bot\"\n\nworkflow w:\n  entry: child\n  child -> done\n",
		"lib/nodes.bot":     "agent a:\n  description: \"x\"\n",
		"kids/k.bot":        "dsl: 2\nimport \"lib/deep.bot\"\n\nworkflow w:\n  entry: done\n",
		"kids/lib/deep.bot": "agent b:\n  description: \"y\"\n",
	}
	req := MaxSyntaxRequirements(files)
	if req.Profile != 2 || !reflect.DeepEqual(req.ImportedBy, []string{"kids/k.bot", "main.bot"}) || !req.UsesImport() {
		t.Fatalf("requirements %+v", req)
	}
	profile := fmt.Sprintf("dsl profile %d", parser.MaxProfile)
	floortest.EachPinOrder(t, parser.ImportSince, func(t *testing.T) {
		// Profile 1 and import alone: the floor is the import release.
		one := SyntaxRequirements{Profile: 1, ImportedBy: []string{"main.bot"}}
		if pf := CheckSyntaxFloor(nil, one); pf.OK || pf.Need != parser.ImportSince || pf.Reason != "import" {
			t.Fatalf("no manifest: %+v", pf)
		}
		// floortest:exempt ImportSince — the floor declared here is the one
		// this case is AT, read from its own pin; the release BELOW it comes
		// from floortest.Below, never from a sibling pin.
		if pf := CheckSyntaxFloor(&Manifest{Requires: &Requires{Iterion: ">= " + parser.ImportSince}}, one); !pf.OK {
			t.Fatalf("the import release declared: %+v", pf)
		}
		if pf := CheckSyntaxFloor(&Manifest{Requires: &Requires{Iterion: ">= " + floortest.Below(t, parser.ImportSince)}}, one); pf.OK {
			t.Fatalf("a release below the import release passed for import: %+v", pf)
		}
		// The newest profile and import: the higher of the two releases,
		// named by a syntax pinned at it. The profile is the one the collapse
		// repoints, so the second order really puts the two on one release.
		both := SyntaxRequirements{Profile: parser.MaxProfile, DeclaredBy: []string{"main.bot"}, ImportedBy: []string{"main.bot"}}
		needsTheHighestPinTouched(t, CheckSyntaxFloor(nil, both), map[string]string{profile: parser.ProfileSince[parser.MaxProfile], "import": parser.ImportSince})
		if want := profile + " (main.bot) and `import` (main.bot)"; both.Describe() != want {
			t.Fatalf("describe: %q, want %q", both.Describe(), want)
		}
		// A profile alone reads as before.
		if pf := CheckProfileFloor(nil, parser.MaxProfile); pf.Need != parser.ProfileSince[parser.MaxProfile] || pf.Reason != profile || pf.OK {
			t.Fatalf("profile alone: %+v", pf)
		}
		if pf := CheckProfileFloor(nil, 1); !pf.OK {
			t.Fatalf("profile 1: %+v", pf)
		}
	})
}
