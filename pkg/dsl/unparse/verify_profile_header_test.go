package unparse

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// `dsl: 1` and no header read alike, and the writer omits profile 1's
// header. A file with no compiled program — half-authored, or an import the
// flat parse cannot resolve — is compared as a mirror of the AST, which
// carried the header's value: every profile-1 file spelled with its header,
// as the author document spells one, was refused as "document.profile is
// missing" the moment it did not compile. The mirror compares the profile
// as the text reads it.
func TestAProfileOneHeaderIsNotLostOnAHalfAuthoredFile(t *testing.T) {
	nl := string(rune(10))
	body := "agent a:" + nl + "  model: m" + nl
	for _, tc := range []struct{ name, src string }{
		{"header, no workflow", "dsl: 1" + nl + nl + body},
		{"header, import, no workflow", "dsl: 1" + nl + "import " + `"lib/x.bot"` + nl + nl + body},
		{"profile 2, no workflow", "dsl: 2" + nl + nl + body},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pr := parser.Parse("x.bot", tc.src)
			if len(pr.Diagnostics) > 0 {
				t.Fatalf("the input is refused: %v", pr.Diagnostics)
			}
			text := Unparse(pr.File)
			if err := Verify(pr.File, text); err != nil {
				t.Fatalf("the written text is refused: %v%s%s", err, nl, text)
			}
		})
	}
}
