package bundle

import (
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A bundle that declares a contract asks for the release that reads
// `contract` — the walk names the files that declare one, in the main or in
// a fragment, and the floor predicate takes the highest release among what
// the sources use; a bundle that uses nothing asks for nothing.
func TestSyntaxRequirementsRecordContractsAndAskForTheirRelease(t *testing.T) {
	files := map[string]string{
		"main.bot":  "import \"lib/c.bot\"\n\nworkflow w:\n  contract: c\n  entry: done\n",
		"lib/c.bot": "contract c:\n  version: 1\n",
	}
	req := MaxSyntaxRequirements(files)
	if !reflect.DeepEqual(req.ContractedBy, []string{"lib/c.bot"}) || !req.UsesContract() || !req.Asks() {
		t.Fatalf("requirements %+v", req)
	}
	one := SyntaxRequirements{Profile: 1, ContractedBy: []string{"main.bot"}}
	if pf := CheckSyntaxFloor(nil, one); pf.OK || pf.Need != parser.ContractSince || pf.Reason != "contract" {
		t.Fatalf("no manifest: %+v", pf)
	}
	if pf := CheckSyntaxFloor(&Manifest{Requires: &Requires{Iterion: ">= " + parser.ContractSince}}, one); !pf.OK {
		t.Fatalf("the contract release declared: %+v", pf)
	}
	if pf := CheckSyntaxFloor(&Manifest{Requires: &Requires{Iterion: ">= " + parser.ImportSince}}, one); pf.OK {
		t.Fatalf("the import release alone passed for a contract: %+v", pf)
	}
	if got := one.Describe(); got != "`contract` (main.bot)" {
		t.Fatalf("describe: %q", got)
	}
	all := SyntaxRequirements{Profile: 2, DeclaredBy: []string{"main.bot"}, ImportedBy: []string{"main.bot"}, ContractedBy: []string{"lib/c.bot"}}
	if pf := CheckSyntaxFloor(nil, all); pf.Need != parser.ContractSince || pf.Reason != "contract" {
		t.Fatalf("profile 2, import and contract: %+v", pf)
	}
	if got := all.Describe(); got != "dsl profile 2 (main.bot) and `import` (main.bot) and `contract` (lib/c.bot)" {
		t.Fatalf("describe: %q", got)
	}
	for _, quiet := range []SyntaxRequirements{{}, {Profile: 1}, {Profile: 1, Unread: []string{"x"}}} {
		if quiet.Asks() {
			t.Fatalf("%+v asks for a floor", quiet)
		}
	}
	// The registry of floors names every pin, so a floor added later is
	// held by the release test and the scaffold alike.
	floors := parser.SyntaxFloors()
	if floors["parser.ContractSince"] != parser.ContractSince || floors["parser.ImportSince"] != parser.ImportSince || floors["parser.ProfileSince[2]"] != parser.ProfileSince[2] {
		t.Fatalf("SyntaxFloors: %v", floors)
	}
}
