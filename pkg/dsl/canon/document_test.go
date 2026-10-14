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
// than written (a prompt body the lexer settles, E053), a YAML comment the
// written form cannot hold — one right after a flow collection's opening
// bracket, which yaml.v3 drops, or one written below the last content line
// that no node holds: the comment of a `...` marker's own line, a block the
// one below the marker displaces, anything under a commented marker line —
// and, before either, a document that does not read.
func TestTheDocumentsCanonicalFormRefusesWhatARewriteWouldLose(t *testing.T) {
	for name, tc := range map[string]struct{ src, want string }{
		"a settled prompt body":                                {"dsl: 2\nprompts:\n  spaced: |\n\n      Indented, after a blank line.\n      Second line.\n\nnodes:\n  - agent: a\n    model: m\n    system: spaced\nworkflow:\n  name: w\n  entry: a\n  edges:\n    - a -> done\n", "E053"},
		"a comment yaml.v3 drops":                              {strings.Replace(looseDoc, "edges: [hello -> done]", "edges: [ # keep me\n    hello -> done]", 1), "(the first: # keep me)"},
		"a comment on a respelled edge":                        {strings.Replace(looseDoc, "edges: [hello -> done]", "edges: [hello->done # keep me\n    ]", 1), "(the first: # keep me)"},
		"a comment on the marker's line":                       {looseDoc + "... # keep me\n", "(the first: # keep me)"},
		"a comment above the marker":                           {looseDoc + "# keep me\n...\n# the foot\n", "(the first: # keep me)"},
		"a comment on the marker's line with a block below it": {looseDoc + "...  # end\n# keep me\n", "(the first: # end)"},
		// Below a BARE marker, the first block rides the document's foot
		// through a rewrite; what is lost is a block a blank line keeps off
		// it, and a second one.
		"a comment below the marker after a blank": {looseDoc + "...\n\n# keep me\n", "(the first: # keep me)"},
		"a second block below the marker":          {looseDoc + "...\n# a\n\n# keep me\n", "(the first: # keep me)"},
		// A document whose last declaration is a block scalar with NO body:
		// the lines below the header are the trailing region's, where the
		// scanner floors a body's indentation at column 1, and the region's
		// own read is what the refusal counts.
		"a comment below a marker under an empty block scalar":      {emptyDoc + "...\n\n# keep me\n", "(the first: # keep me)"},
		"a comment below two markers under an empty block scalar":   {emptyDoc + "...\n...\n# keep me\n", "(the first: # keep me)"},
		"a second block below a marker under an empty block scalar": {emptyDoc + "...\n# a\n\n# keep me\n", "(the first: # keep me)"},
		"a comment below a marker under an empty tool script":       {emptyScriptDoc + "...\n\n# keep me\n", "(the first: # keep me)"},
		// A block above the marker a blank keeps off the node AND the one
		// below the marker displaces off the document's foot is hung on
		// nothing: the source is the only place it is written.
		"a block above the marker the one below it displaces": {looseDoc + "\n# keep me\n\n...\n# the foot\n", "(the first: # keep me)"},
		// Indented and displaced off the document's foot, a block is read
		// at its own column or not at all: this document's last
		// declaration is a flow collection, so no node of it holds that
		// line either, and a rewrite would delete it.
		"an indented block above the marker the one below displaces": {looseDoc + "  # keep me\n...\n# the foot\n", "(the first: # keep me)"},
		"the same text above the marker and on its line":             {looseDoc + "\n# keep me\n\n...  # keep me\n", "(the first: # keep me)"},
		// The same text as a MID-DOCUMENT foot comment and on the marker's
		// line: two comments, and only the first has a node to ride. Read
		// as one, the marker's line is deleted and nothing is refused.
		"the text of a mid-document foot on the marker's line": {footTwinDoc + "...  # twice\n", "(the first: # twice)"},
		"the text of a mid-document foot below a blank":        {footTwinDoc + "...\n\n# twice\n", "(the first: # twice)"},
		// The directive LINE is dropped with no refusal, as the markers
		// are; a comment written on one is counted like any other, and the
		// written form has nowhere to put it.
		"a comment on a directive's line": {"%YAML 1.1 # keep me\n---\n" + looseDoc, "(the first: # keep me)"},
		"a document that does not read":   {"dsl: 2\nnodes: 3\n", "does not read"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Document("x.bot.yaml", []byte(tc.src))
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want a refusal saying %q", err, tc.want)
			}
		})
	}
}
