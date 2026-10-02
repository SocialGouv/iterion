package author

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// A prompt written in place of its reference is read from a block scalar as
// a declared prompt's body is: the block's clip newline is the scalar's, not
// the author's, and is dropped — the same authored text reaches the model
// the same whether it is declared or written in place. A quoted body's
// trailing newline is the author's character and is kept; the writer
// spells such a body quoted, so the round trip keeps it.
func TestAPromptInPlaceDropsTheClipNewlineAsADeclaredOneDoes(t *testing.T) {
	nl, bs := string(rune(10)), string(rune(92))
	tail := strings.Join([]string{"workflow:", "  name: w", "  entry: a", "  edges:", "    - a -> done", ""}, nl)
	for _, tc := range []struct {
		name, scalar, want string
	}{
		{"a literal block, clip", "|" + nl + "      first" + nl + "      second" + nl, "first" + nl + "second"},
		{"a literal block, strip", "|-" + nl + "      first" + nl + "      second" + nl, "first" + nl + "second"},
		// keep: the final line break is the scalar's, the blank lines after it are the author's and stay
		{"a literal block, keep, one blank line", "|+" + nl + "      first" + nl + "      second" + nl + nl, "first" + nl + "second" + nl},
		{"a literal block, keep, two blank lines", "|+" + nl + "      first" + nl + "      second" + nl + nl + nl, "first" + nl + "second" + nl + nl},
		{"a folded block", ">" + nl + "      first" + nl + "      second" + nl, "first second"},
		{"a quoted body ending with the author's newline", `"first` + bs + `nsecond` + bs + `n"` + nl, "first" + nl + "second" + nl},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := strings.Join([]string{"dsl: 2", "prompts:", "  p: " + tc.scalar + "nodes:", "  - agent: a", "    model: m", "    user: " + tc.scalar + "    system: p", ""}, nl) + tail
			res := Parse("p.yaml", []byte(doc))
			if res.HasErrors() {
				t.Fatalf("the document is refused: %v", res.Diagnostics)
			}
			inPlace := inlineBody(t, res, res.File.Agents[0].User)
			if inPlace != tc.want {
				t.Errorf("the body written in place is read as %q, want %q", inPlace, tc.want)
			}
			declared := inlineBody(t, res, "p")
			if want := parser.CanonicalPromptBodyIn(2, tc.want); declared != want {
				t.Errorf("the declared body is read as %q, want %q", declared, want)
			}
		})
	}
}

// The round trip of a prompt written in place keeps a body that ends with a
// newline the author wrote: the writer spells it quoted, a block scalar's
// clip would drop it on the way back.
func TestAPromptInPlaceEndingWithANewlineRoundTrips(t *testing.T) {
	nl := string(rune(10))
	for _, body := range []string{"first" + nl + "second" + nl, "first" + nl + "second", "one line" + nl} {
		t.Run(strings.ReplaceAll(body, nl, "/"), func(t *testing.T) {
			bot := strings.Join([]string{"dsl: 2", "", "agent a:", "  model: m", "  user: " + unparse.QuoteStrict(body), "", "workflow w:", "  entry: a", "  a -> done", ""}, nl)
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
			if got := inlineBody(t, res, res.File.Agents[0].User); got != body {
				t.Errorf("the body came back as %q, want %q%s--- written:%s%s", got, body, nl, nl, out)
			}
		})
	}
}
