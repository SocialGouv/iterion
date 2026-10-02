package bundlelint

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// C252's whole value is the sentence an author reads, and this floor's
// failure mode is the one that is SILENT: an older runner parses `tools: []`
// without complaint and reads it as an absent list — the opposite bound.
// Inheriting the generic consequence ("re-parses a subbot child … and fails
// at that parse") would tell the author to expect a crash that never comes.
//
// Reddens on the mutation that drops the per-floor arm.
func TestC252NamesTheInversionRatherThanAParseFailureForAnEmptyToolList(t *testing.T) {
	find := func(diags []Diag) *Diag {
		for i := range diags {
			if diags[i].Code == DiagProfileNeedsFloor {
				return &diags[i]
			}
		}
		return nil
	}
	req := bundle.SyntaxRequirements{Profile: 1, EmptyToolsBy: []string{"main.bot"}}
	d := find(CheckConsistency(Input{Manifest: nil, Syntax: req, EngineBuild: "v" + bundle.DeclaredEmptyToolsSince}))
	if d == nil {
		t.Fatal("a bundle spelling `tools: []` with no floor draws no C252")
	}
	if !strings.Contains(d.Message, "empty `tools: []` declaration (main.bot)") {
		t.Errorf("C252 does not name the syntax or the file: %q", d.Message)
	}
	if !strings.Contains(d.Message, "ABSENT list") || !strings.Contains(d.Message, "opposite bound") {
		t.Errorf("C252 does not name the inversion: %q", d.Message)
	}
	if strings.Contains(d.Message, "fails at that parse") {
		t.Errorf("C252 inherited the generic parse-failure consequence, which is false for this floor: %q", d.Message)
	}
	if !strings.Contains(d.Hint, `">= `+bundle.DeclaredEmptyToolsSince+`"`) {
		t.Errorf("C252 does not name the release: %q", d.Hint)
	}
	// Declared at the floor: silent.
	ok := &bundle.Manifest{Name: "probe", Requires: &bundle.Requires{Iterion: ">= " + bundle.DeclaredEmptyToolsSince}}
	if d := find(CheckConsistency(Input{Manifest: ok, Syntax: req, EngineBuild: "v" + bundle.DeclaredEmptyToolsSince})); d != nil {
		t.Errorf("C252 drawn with the floor declared: %+v", *d)
	}
}
