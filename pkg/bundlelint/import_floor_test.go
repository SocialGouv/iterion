package bundlelint

import (
	"fmt"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/floortest"
	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A bundle that imports needs the release that reads `import`, whatever its
// profile: no floor, or one below it, draws C252 naming that release; a floor
// at or above it draws nothing.
//
// The cases run in both pin orders: the day `import` and the profile are cut
// in one release, a floor written as the profile's pin is the import release
// itself, and the case would be asking C252 to refuse what it must accept.
func TestABundleThatImportsNeedsTheImportRelease(t *testing.T) {
	find := func(diags []Diag) *Diag {
		for i := range diags {
			if diags[i].Code == DiagProfileNeedsFloor {
				return &diags[i]
			}
		}
		return nil
	}
	profile := fmt.Sprintf("dsl profile %d", parser.MaxProfile)
	floortest.EachPinOrder(t, parser.ImportSince, func(t *testing.T) {
		none := CheckConsistency(Input{Manifest: nil, Syntax: bundle.SyntaxRequirements{Profile: 1, ImportedBy: []string{"main.bot"}}, EngineBuild: "v" + parser.ImportSince})
		if d := find(none); d == nil || !strings.Contains(d.Message, "`import` (main.bot)") || !strings.Contains(d.Hint, `">= `+parser.ImportSince+`"`) {
			t.Fatalf("no floor: %v", none)
		}
		low := &bundle.Manifest{Name: "probe", Requires: &bundle.Requires{Iterion: ">= " + floortest.Below(t, parser.ImportSince)}}
		// The newest profile is the one the collapse repoints, so the second
		// order really puts the profile and `import` on one release.
		below := CheckConsistency(Input{Manifest: low, Syntax: bundle.SyntaxRequirements{Profile: parser.MaxProfile, DeclaredBy: []string{"main.bot"}, ImportedBy: []string{"lib/nodes.bot"}}, EngineBuild: "v" + parser.ImportSince})
		d := find(below)
		if d == nil || !strings.Contains(d.Message, profile+" (main.bot) and `import` (lib/nodes.bot)") || !strings.Contains(d.Message, parser.ImportSince) {
			t.Fatalf("a floor below the import release: %v", below)
		}
		ok := &bundle.Manifest{Name: "probe", Requires: &bundle.Requires{Iterion: ">= " + parser.ImportSince}}
		if d := find(CheckConsistency(Input{Manifest: ok, Syntax: bundle.SyntaxRequirements{Profile: 1, ImportedBy: []string{"main.bot"}}, EngineBuild: "v" + parser.ImportSince})); d != nil {
			t.Fatalf("C252 drawn with the import release declared: %+v", *d)
		}
	})
}
