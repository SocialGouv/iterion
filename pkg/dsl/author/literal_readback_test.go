package author

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// A string value that spans lines is written as a literal block — unless
// that spelling does not read back as the value: yaml.v3's emitter writes a
// block that opens with an empty line one line short, so a command or a
// description starting with a newline came back without it, silently, on
// every value of the document. Such a value is written quoted.
func TestAValueWhoseLiteralSpellingDoesNotReadBackIsWrittenQuoted(t *testing.T) {
	nl := string(rune(10))
	for _, tc := range []struct{ name, value string }{
		{"a leading newline", nl + "echo a"},
		{"two leading newlines", nl + nl + "echo a"},
		{"a leading and a trailing newline", nl + "echo a" + nl},
		{"only newlines", nl + nl},
		{"a plain two-line value, still a block", "echo a" + nl + "echo b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bot := strings.Join([]string{"dsl: 2", "", "tool t:", "  command: " + unparse.QuoteStrict(tc.value), "", "workflow w:", "  entry: t", "  t -> done", ""}, nl)
			pr := parser.Parse("x.bot", bot)
			if len(pr.Diagnostics) > 0 {
				t.Fatalf(".bot refused: %v", pr.Diagnostics)
			}
			out, err := Write(pr.File)
			if err != nil {
				t.Fatal(err)
			}
			res := Parse("x.yaml", out)
			if res.HasErrors() {
				t.Fatalf("the document is refused: %v%s%s", res.Diagnostics, nl, out)
			}
			if got := res.File.Tools[0].Command; got != tc.value {
				t.Errorf("the command came back as %q, want %q%s--- written:%s%s", got, tc.value, nl, nl, out)
			}
			if tc.name == "a plain two-line value, still a block" && !strings.Contains(string(out), "command: |") {
				t.Errorf("a value that reads back keeps its literal block:%s%s", nl, out)
			}
		})
	}
}
