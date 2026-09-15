package bundle

import (
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A bot in several files asks for the release that reads `import` — the
// walk names the files that import, and the floor predicate takes the
// highest release among what the sources use.
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
	// Profile 1 and import alone: the floor is the import release.
	one := SyntaxRequirements{Profile: 1, ImportedBy: []string{"main.bot"}}
	if pf := CheckSyntaxFloor(nil, one); pf.OK || pf.Need != parser.ImportSince || pf.Reason != "import" {
		t.Fatalf("no manifest: %+v", pf)
	}
	if pf := CheckSyntaxFloor(&Manifest{Requires: &Requires{Iterion: ">= " + parser.ImportSince}}, one); !pf.OK {
		t.Fatalf("the import release declared: %+v", pf)
	}
	if pf := CheckSyntaxFloor(&Manifest{Requires: &Requires{Iterion: ">= 3.141.0"}}, one); pf.OK {
		t.Fatalf("the profile release alone passed for import: %+v", pf)
	}
	// Profile 2 and import: the higher of the two releases.
	both := SyntaxRequirements{Profile: 2, DeclaredBy: []string{"main.bot"}, ImportedBy: []string{"main.bot"}}
	if pf := CheckSyntaxFloor(nil, both); pf.Need != parser.ImportSince || pf.Reason != "import" {
		t.Fatalf("profile 2 and import: %+v", pf)
	}
	if both.Describe() != "dsl profile 2 (main.bot) and `import` (main.bot)" {
		t.Fatalf("describe: %q", both.Describe())
	}
	// A profile alone reads as before.
	if pf := CheckProfileFloor(nil, 2); pf.Need != parser.ProfileSince[2] || pf.Reason != "dsl profile 2" || pf.OK {
		t.Fatalf("profile alone: %+v", pf)
	}
	if pf := CheckProfileFloor(nil, 1); !pf.OK {
		t.Fatalf("profile 1: %+v", pf)
	}
}
