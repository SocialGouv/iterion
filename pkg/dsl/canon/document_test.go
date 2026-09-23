package canon

import (
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/author"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// looseDoc is an author document in a form the writer does not produce —
// flow mappings, a sequence at the margin — describing one agent.
const looseDoc = "dsl: 2\nprompts: {ask: Say hello.}\nnodes:\n- agent: hello\n  model: m\n  system: ask\nworkflow: {name: hello, entry: hello, edges: [hello -> done]}\n"

// The canonical form of a document is the author writer's text, proven the
// same program, on the document's own bytes: a BOM and CRLF line endings are
// kept as they are for a .bot, and a canonical document written with them is
// its own canonical form.
func TestTheDocumentsCanonicalFormKeepsTheBOMAndTheLineEndings(t *testing.T) {
	want, err := author.Write(author.Parse("x.bot.yaml", []byte(looseDoc)).File)
	if err != nil {
		t.Fatal(err)
	}
	src := "\ufeff" + strings.ReplaceAll(looseDoc, "\n", "\r\n")
	out, err := Document("x.bot.yaml", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if got != "\ufeff"+strings.ReplaceAll(string(want), "\n", "\r\n") {
		t.Fatalf("not the writer's text on the file's own bytes:\n%q\nwant\n%q", got, want)
	}
	again, err := Document("x.bot.yaml", out)
	if err != nil || string(again) != got {
		t.Fatalf("a canonical document with a BOM and CRLF is not its own canonical form: %v\n%q", err, again)
	}
	plain, err := Document("x.bot.yaml", []byte(looseDoc))
	if err != nil || string(plain) != string(want) {
		t.Fatalf("an LF document gained a BOM or a CR: %v\n%q", err, plain)
	}
	if unparse.Unparse(author.Parse("x.bot.yaml", plain).File) != unparse.Unparse(author.Parse("x.bot.yaml", []byte(looseDoc)).File) {
		t.Fatal("the canonical document is another program")
	}
}

// The canonical form is refused, the bytes left the author's, when the
// rewrite would lose what the author wrote: a text the .bot reads otherwise
// than written (a prompt body the lexer settles, E053), a YAML comment —
// and, before either, a document that does not read.
func TestTheDocumentsCanonicalFormRefusesWhatARewriteWouldLose(t *testing.T) {
	for name, tc := range map[string]struct{ src, want string }{
		"a settled prompt body":         {"dsl: 2\nprompts:\n  spaced: |\n\n      Indented, after a blank line.\n      Second line.\n\nnodes:\n  - agent: a\n    model: m\n    system: spaced\nworkflow:\n  name: w\n  entry: a\n  edges:\n    - a -> done\n", "E053"},
		"a YAML comment":                {"# keep me\n" + looseDoc, "YAML comment"},
		"a document that does not read": {"dsl: 2\nnodes: 3\n", "does not read"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Document("x.bot.yaml", []byte(tc.src))
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want a refusal saying %q", err, tc.want)
			}
		})
	}
}
