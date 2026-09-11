package parser

import (
	"strings"
	"testing"
)

// Profile 2 reads standard escapes by itself: the profile-1 directive has
// nothing left to switch on, and a copy left behind — above the header or
// below it — is refused at its own line rather than read as a harmless
// comment that a later reader might honour differently.
func TestDirectiveIsRefusedInProfileTwo(t *testing.T) {
	cases := []struct {
		name string
		src  string
		line int
	}{
		{"above the header", "## strict-escape: on\ndsl: 2\ntool t:\n  command: \"a\\nb\"\n", 1},
		{"below the header", "dsl: 2\n## strict-escape: on\ntool t:\n  command: \"a\\nb\"\n", 2},
		{"single hash", "dsl: 2\n\n# strict-escape:on\ntool t:\n  command: \"a\\nb\"\n", 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := Parse("x.bot", c.src)
			var found []Diagnostic
			for _, d := range res.Diagnostics {
				if d.Code == DiagDirectiveInProfile {
					found = append(found, d)
				}
			}
			if len(found) != 1 {
				t.Fatalf("want one E042, got %v", res.Diagnostics)
			}
			if found[0].Line != c.line {
				t.Fatalf("E042 at line %d, want %d", found[0].Line, c.line)
			}
			if !strings.Contains(found[0].Hint, "delete") {
				t.Fatalf("E042 arrives without its remedy: %q", found[0].Hint)
			}
			// The string was still read once, in the profile's mode.
			if got := res.File.Tools[0].Command; got != "a\nb" {
				t.Fatalf("command read as %q under profile 2", got)
			}
		})
	}
	// Profile 1 keeps honouring the directive, silently.
	res := Parse("x.bot", "## strict-escape: on\ntool t:\n  command: \"a\\nb\"\n")
	if len(res.Diagnostics) != 0 || res.File.Tools[0].Command != "a\nb" {
		t.Fatalf("profile 1 with the directive: %v, command %q", res.Diagnostics, res.File.Tools[0].Command)
	}
}
