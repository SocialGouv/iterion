package unparse

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// A profile-1 file opts into standard escapes by the strict-escape
// directive, which profile 1 reads among the first lines of the file only
// — a frozen rule (parser.Preamble). The writer places the directive under
// the frontmatter, which the catalog reader wants first; a frontmatter long
// enough pushes it out of that window, where it is in the text and not in
// effect, and a value only the strict form can hold re-reads as another
// program. The guard refuses such a text by its cause, not as a node that
// differs — and does not refuse a text that reads the same all the same:
// a stray directive comment the author wrote, hoisted by the writer under a
// long frontmatter, with values both modes read alike.
func TestAFrontmatterCannotPushTheDirectiveOutOfItsWindow(t *testing.T) {
	nl := string(rune(10))
	bq := string(rune(96))
	strictOnly := "a" + bq + "b" + nl + "c" // no v1 form: a backtick and a newline
	for _, tc := range []struct {
		name   string
		lines  int
		stray  bool // a directive comment written on the tool, where profile 1 does not read it
		value  string
		refuse bool
	}{
		{"short frontmatter, strict value", 3, false, strictOnly, false},
		{"long frontmatter, strict value", 40, false, strictOnly, true},
		{"long frontmatter, stray directive, plain value", 40, true, "x", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			b.WriteString("## ---" + nl + "## name: probe" + nl + "## description: |" + nl)
			for i := 0; i < tc.lines; i++ {
				b.WriteString("##   a line of the description" + nl)
			}
			b.WriteString("## ---" + nl + nl)
			if tc.stray {
				b.WriteString("## strict-escape: on" + nl)
			}
			b.WriteString("tool t:" + nl + "  command: " + `"x"` + nl + nl + "workflow w:" + nl + "  entry: t" + nl + "  t -> done" + nl)
			pr := parser.Parse("x.bot", b.String())
			if len(pr.Diagnostics) > 0 {
				t.Fatalf("the input is refused: %v", pr.Diagnostics)
			}
			pr.File.Tools[0].Command = tc.value
			text := Unparse(pr.File)
			err := Verify(pr.File, text)
			switch {
			case !tc.refuse && err != nil:
				t.Fatalf("a text that reads as the same program is refused: %v%s%s", err, nl, text)
			case tc.refuse && err == nil:
				t.Fatalf("a text whose directive is out of the window is accepted:%s%s", nl, text)
			case tc.refuse && !strings.Contains(err.Error(), "strict-escape directive"):
				t.Fatalf("the refusal does not name its cause: %v", err)
			}
		})
	}
}
