package bundlelint

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// A bundle whose sources spell an alias in a tool list draws C252 naming the
// alias consequence — a runner below the alias release keeps the old
// resolver, so the tool list fails at dispatch — until the manifest declares
// requires.iterion at or above bundle.ToolAliasesSince, where the same
// declaration is what turns the resolver on.
func TestAnAliasBundleIsAskedForTheAliasFloor(t *testing.T) {
	find := func(diags []Diag) *Diag {
		for i := range diags {
			if diags[i].Code == DiagProfileNeedsFloor {
				return &diags[i]
			}
		}
		return nil
	}
	req := bundle.SyntaxRequirements{Profile: 1, AliasBy: []string{"main.bot"}}
	none := CheckConsistency(Input{Manifest: nil, Syntax: req, EngineBuild: "v3.176.0"})
	d := find(none)
	if d == nil || !strings.Contains(d.Message, "the Claw tool alias (main.bot)") || !strings.Contains(d.Message, "tool list fails at dispatch") || !strings.Contains(d.Hint, `">= `+bundle.ToolAliasesSince+`"`) {
		t.Fatalf("no floor: %v", d)
	}
	below := &bundle.Manifest{Name: "probe", Requires: &bundle.Requires{Iterion: ">= 3.176.0"}}
	if d = find(CheckConsistency(Input{Manifest: below, Syntax: req, EngineBuild: "v3.176.0"})); d == nil || !strings.Contains(d.Message, "tool list fails at dispatch") || !strings.Contains(d.Hint, `">= `+bundle.ToolAliasesSince+`"`) {
		t.Fatalf("a floor below the alias release: %v", d)
	}
	ok := &bundle.Manifest{Name: "probe", Requires: &bundle.Requires{Iterion: ">= " + bundle.ToolAliasesSince}}
	if d := find(CheckConsistency(Input{Manifest: ok, Syntax: req, EngineBuild: "v3.176.0"})); d != nil {
		t.Fatalf("C252 drawn with the alias release declared: %+v", *d)
	}
}
