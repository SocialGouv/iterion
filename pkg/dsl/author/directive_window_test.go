package author

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// A profile-1 document is spelled with standard escapes, which profile 1
// reads by the strict-escape directive — among the first lines of the file
// only, a frozen rule (parser.Preamble). The catalog is comments too, of
// any length: written above the directive, forty lines of description
// pushed it out of that window, and every escape of the text was read as
// its two characters, without a word. The directive is read whatever the
// catalog's length, and the catalog identity survives the .bot the
// document writes back.
func TestTheDirectiveIsReadWhateverTheCatalogsLength(t *testing.T) {
	nl := string(rune(10))
	bs := string(rune(92))
	want := "printf a" + nl + "b"
	for _, lines := range []int{1, 40} {
		desc := strings.Repeat("    a line of the description"+nl, lines)
		doc := "dsl: 1" + nl +
			"catalog:" + nl +
			"  name: probe" + nl +
			"  description: |" + nl + desc +
			"nodes:" + nl +
			"  - tool: t" + nl +
			"    command: " + `"printf a` + bs + `nb"` + nl +
			"workflow:" + nl +
			"  name: w" + nl +
			"  entry: t" + nl +
			"  edges:" + nl +
			"    - t -> done" + nl
		res := Parse("x.yaml", []byte(doc))
		if res.HasErrors() {
			t.Fatalf("%d-line description: the document is refused: %v", lines, res.Diagnostics)
		}
		if !parser.ReadPreamble(res.Text).StrictEscape {
			t.Errorf("%d-line description: the parser does not read the directive off the spelled text:%s%s", lines, nl, res.Text)
		}
		if got := res.File.Tools[0].Command; got != want {
			t.Errorf("%d-line description: the command is read as %q, want %q", lines, got, want)
		}
		bot := unparse.Unparse(res.File)
		if fm := bundle.ParseFrontmatter([]byte(bot)); fm == nil || fm.Name != "probe" {
			t.Errorf("%d-line description: the catalog identity is lost in the .bot the document writes back:%s%s", lines, nl, bot)
		}
	}
}
