package bundlelint

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// A bundle written in profile 2 with no declared engine floor draws C252,
// naming the files that declare the profile and, on an orderable build,
// the value to declare; a declared floor, or profile 1, draws nothing.
func TestProfileTwoWithoutAFloorDrawsC252(t *testing.T) {
	m := &bundle.Manifest{Name: "probe"}
	diags := CheckConsistency(Input{Manifest: m, SyntaxProfile: 2, ProfileDeclaredBy: []string{"children/a.bot"}, EngineBuild: "v3.141.0+abc"})
	var found *Diag
	for i := range diags {
		if diags[i].Code == DiagProfileNeedsFloor {
			found = &diags[i]
		}
	}
	if found == nil {
		t.Fatalf("no C252 in %v", diags)
	}
	if found.Severity != SeverityWarning || !strings.Contains(found.Message, "children/a.bot") || !strings.Contains(found.Hint, `">= 3.141.0"`) {
		t.Fatalf("C252 = %+v", *found)
	}
	// A dev build still names the release that reads the profile.
	diags = CheckConsistency(Input{Manifest: m, SyntaxProfile: 2, ProfileDeclaredBy: []string{"main.bot"}, EngineBuild: "dev"})
	if len(diags) != 1 || diags[0].Code != DiagProfileNeedsFloor || !strings.Contains(diags[0].Hint, `">= 3.141.0"`) {
		t.Fatalf("dev build: %v", diags)
	}
	// With a floor, or in profile 1, nothing.
	withFloor := &bundle.Manifest{Name: "probe", Requires: &bundle.Requires{Iterion: ">= 3.141.0"}}
	for _, in := range []Input{
		{Manifest: withFloor, SyntaxProfile: 2, ProfileDeclaredBy: []string{"main.bot"}, EngineBuild: "v3.141.0"},
		{Manifest: m, SyntaxProfile: 1, EngineBuild: "v3.141.0"},
		{Manifest: m, EngineBuild: "v3.141.0"},
	} {
		for _, d := range CheckConsistency(in) {
			if d.Code == DiagProfileNeedsFloor {
				t.Fatalf("C252 drawn for %+v", in)
			}
		}
	}
}
