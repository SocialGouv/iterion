package bundlelint

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// A declared floor below the release that reads the profile draws C252
// naming both; one at or above it draws nothing — presence was never the
// question.
func TestAFloorBelowTheProfilesReleaseDrawsC252(t *testing.T) {
	low := &bundle.Manifest{Name: "probe", Requires: &bundle.Requires{Iterion: ">= 0.0.1"}}
	diags := CheckConsistency(Input{Manifest: low, SyntaxProfile: 2, ProfileDeclaredBy: []string{"kids/c.bot"}, EngineBuild: "v3.141.0"})
	var found *Diag
	for i := range diags {
		if diags[i].Code == DiagProfileNeedsFloor {
			found = &diags[i]
		}
	}
	if found == nil {
		t.Fatalf("no C252 for a floor below the release: %v", diags)
	}
	if !strings.Contains(found.Message, `">= 0.0.1"`) || !strings.Contains(found.Message, "3.141.0") || !strings.Contains(found.Hint, `">= 3.141.0"`) {
		t.Fatalf("C252 = %+v", *found)
	}
	for _, req := range []string{">= 3.141.0", ">= 3.150.0", "3.141.0"} {
		m := &bundle.Manifest{Name: "probe", Requires: &bundle.Requires{Iterion: req}}
		for _, d := range CheckConsistency(Input{Manifest: m, SyntaxProfile: 2, ProfileDeclaredBy: []string{"main.bot"}, EngineBuild: "v3.141.0"}) {
			if d.Code == DiagProfileNeedsFloor {
				t.Fatalf("C252 drawn for %q", req)
			}
		}
	}
}

// A bundle with no manifest at all — known by its skills/ — still draws
// C252 for its profile: there is no floor, and no place for one yet.
func TestAProfileTwoBundleWithoutAManifestDrawsC252(t *testing.T) {
	diags := CheckConsistency(Input{Manifest: nil, SyntaxProfile: 2, ProfileDeclaredBy: []string{"main.bot"}, EngineBuild: "v3.141.0"})
	if len(diags) != 1 || diags[0].Code != DiagProfileNeedsFloor {
		t.Fatalf("no manifest: %v", diags)
	}
	// And nothing else runs without a manifest — nor anything at all in profile 1.
	if diags := CheckConsistency(Input{Manifest: nil, SyntaxProfile: 1, EngineBuild: "v3.141.0"}); len(diags) != 0 {
		t.Fatalf("profile 1, no manifest: %v", diags)
	}
}

// A child the walk could not read is named (C253), so profile 1 is never
// taken for "checked".
func TestAnUnreadChildDrawsC253(t *testing.T) {
	m := &bundle.Manifest{Name: "probe"}
	diags := CheckConsistency(Input{Manifest: m, SyntaxProfile: 1, ProfileUnread: []string{"../sib/main.bot"}, EngineBuild: "v3.141.0"})
	if len(diags) != 1 || diags[0].Code != DiagProfileChildUnread || !strings.Contains(diags[0].Message, "../sib/main.bot") {
		t.Fatalf("unread child: %v", diags)
	}
	if diags := CheckConsistency(Input{Manifest: m, SyntaxProfile: 1, EngineBuild: "v3.141.0"}); len(diags) != 0 {
		t.Fatalf("nothing unread: %v", diags)
	}
}
