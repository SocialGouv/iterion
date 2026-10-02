package bundlelint

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A bot in several files asks for the release that reads `import`, whatever
// its profile: no floor, or one below it, draws C252 naming that release;
// a floor at or above it draws nothing.
func TestABundleThatImportsNeedsTheImportRelease(t *testing.T) {
	find := func(diags []Diag) *Diag {
		for i := range diags {
			if diags[i].Code == DiagProfileNeedsFloor {
				return &diags[i]
			}
		}
		return nil
	}
	none := CheckConsistency(Input{Manifest: nil, Syntax: bundle.SyntaxRequirements{Profile: 1, ImportedBy: []string{"main.bot"}}, EngineBuild: "v" + parser.ImportSince})
	if d := find(none); d == nil || !strings.Contains(d.Message, "`import` (main.bot)") || !strings.Contains(d.Hint, `">= `+parser.ImportSince+`"`) {
		t.Fatalf("no floor: %v", none)
	}
	low := &bundle.Manifest{Name: "probe", Requires: &bundle.Requires{Iterion: ">= " + parser.ProfileSince[2]}}
	below := CheckConsistency(Input{Manifest: low, Syntax: bundle.SyntaxRequirements{Profile: 2, DeclaredBy: []string{"main.bot"}, ImportedBy: []string{"lib/nodes.bot"}}, EngineBuild: "v" + parser.ImportSince})
	d := find(below)
	if d == nil || !strings.Contains(d.Message, "dsl profile 2 (main.bot) and `import` (lib/nodes.bot)") || !strings.Contains(d.Message, parser.ImportSince) {
		t.Fatalf("a floor for the profile alone: %v", below)
	}
	ok := &bundle.Manifest{Name: "probe", Requires: &bundle.Requires{Iterion: ">= " + parser.ImportSince}}
	if d := find(CheckConsistency(Input{Manifest: ok, Syntax: bundle.SyntaxRequirements{Profile: 1, ImportedBy: []string{"main.bot"}}, EngineBuild: "v" + parser.ImportSince})); d != nil {
		t.Fatalf("C252 drawn with the import release declared: %+v", *d)
	}
}
