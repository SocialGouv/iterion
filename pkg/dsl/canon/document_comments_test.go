package canon

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/author"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// commentedDoc carries a head comment, a line comment on a property, a
// comment above a node and a foot comment, in a form the writer does not
// produce: the sequences sit at the margin.
const commentedDoc = `# keep me: the document's head
dsl: 2
prompts:
  ask: Say hello. # and me, on the property
nodes:
# above the node
- agent: hello
  model: m
  system: ask
workflow:
  name: hello
  entry: hello
  edges:
  - hello -> done
# the foot
`

// A commented document is rewritten in its canonical form with every
// comment where the author wrote it — on the node it hangs on — and the
// result is its own canonical form, the same program.
func TestTheDocumentsCanonicalFormKeepsItsComments(t *testing.T) {
	out, err := Document("x.bot.yaml", []byte(commentedDoc))
	if err != nil {
		t.Fatalf("a commented document was refused: %v", err)
	}
	got := string(out)
	if got == commentedDoc {
		t.Fatal("the fixture is already canonical: it proves nothing about a rewrite")
	}
	want := `# keep me: the document's head
dsl: 2
prompts:
  ask: Say hello. # and me, on the property
nodes:
  # above the node
  - agent: hello
    model: m
    system: ask
workflow:
  name: hello
  entry: hello
  edges:
    - hello -> done
# the foot
`
	if got != want {
		t.Fatalf("the comments are not where they were written:\n%s\nwant\n%s", got, want)
	}
	again, err := Document("x.bot.yaml", out)
	if err != nil || string(again) != got {
		t.Fatalf("the rewritten document is not its own canonical form: %v\n%s", err, again)
	}
	if unparse.Unparse(author.Parse("x.bot.yaml", out).File) != unparse.Unparse(author.Parse("x.bot.yaml", []byte(commentedDoc)).File) {
		t.Fatal("the rewritten document is another program")
	}
}

// A comment rides its node, never its position: the writer orders a node's
// keys by the registry, and the comment of a key the author wrote last
// stays on that key.
func TestADocumentsCommentFollowsItsKey(t *testing.T) {
	src := strings.Replace(commentedDoc, "  model: m\n  system: ask\n", "  system: ask # the system prompt\n  model: m # the model\n", 1)
	out, err := Document("x.bot.yaml", []byte(src))
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	for _, line := range []string{"    model: m # the model\n", "    system: ask # the system prompt\n"} {
		if !strings.Contains(string(out), line) {
			t.Fatalf("%q is not on its key:\n%s", line, out)
		}
	}
}

// A ` #` ending a plain value is kept only while the written form reads the
// same value cut by the same comment: a reading the written form no longer
// reports — the value quoted, the comment moved off it — is named.
func TestACommentReadingTheWrittenFormLosesIsNamed(t *testing.T) {
	src := author.Parse("x.bot.yaml", []byte(commentedDoc)).Diagnostics
	var readings int
	for _, d := range src {
		if author.IsCommentReading(d) {
			readings++
		}
	}
	if readings != 1 {
		t.Fatalf("the fixture carries %d comment reading(s), want 1: %v", readings, src)
	}
	if why := commentReadingLost(src, src); why != "" {
		t.Fatalf("a written form reporting the same reading lost it: %s", why)
	}
	if why := commentReadingLost(src, nil); !strings.Contains(why, `"Say hello."`) {
		t.Fatalf("a reading the written form lost is not named: %q", why)
	}
	other := []parser.Diagnostic{{Code: parser.DiagAuthorPromptBody, Severity: parser.SeverityWarning, Message: "a prompt body the lexer settles"}}
	if author.IsCommentReading(other[0]) || commentReadingLost(other, nil) != "" {
		t.Fatal("an E053 reading of another kind is taken for a comment reading")
	}
}
