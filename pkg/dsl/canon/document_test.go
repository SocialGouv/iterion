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
// bracket, which yaml.v3 drops, or one on a respelled edge — and, before
// either, a document that does not read.
//
// The lines written around a `...` or `---` marker were fourteen rows of
// this table until #2359: the writer re-emits those markers with the region
// they sit in, so each of those rows is now a KEEP row, named one by one in
// TestTheDocumentsCanonicalFormKeepsTheLinesWrittenAroundAMarker. A
// fifteenth row left too — the comment on a `%YAML` directive's line — for
// TestTheDocumentsCanonicalFormRefusesTheStreamDirectiveItCannotEmit, which
// refuses it by naming the DIRECTIVE rather than the comment. The count
// below is announced so neither table can lose a row in silence (#1784).
func TestTheDocumentsCanonicalFormRefusesWhatARewriteWouldLose(t *testing.T) {
	table := map[string]struct{ src, want string }{
		"a settled prompt body":         {"dsl: 2\nprompts:\n  spaced: |\n\n      Indented, after a blank line.\n      Second line.\n\nnodes:\n  - agent: a\n    model: m\n    system: spaced\nworkflow:\n  name: w\n  entry: a\n  edges:\n    - a -> done\n", "E053"},
		"a comment yaml.v3 drops":       {strings.Replace(looseDoc, "edges: [hello -> done]", "edges: [ # keep me\n    hello -> done]", 1), "(the first: # keep me)"},
		"a comment on a respelled edge": {strings.Replace(looseDoc, "edges: [hello -> done]", "edges: [hello->done # keep me\n    ]", 1), "(the first: # keep me)"},
		"a document that does not read": {"dsl: 2\nnodes: 3\n", "does not read"},
	}
	if len(table) != 4 {
		t.Fatalf("the table holds %d rows, want 4 — the fourteen marker rows are KEEP rows since #2359, and a row does not leave this table in silence", len(table))
	}
	for name, tc := range table {
		t.Run(name, func(t *testing.T) {
			_, err := Document("x.bot.yaml", []byte(tc.src))
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want a refusal saying %q", err, tc.want)
			}
		})
	}
}

// A `%YAML` or `%TAG` directive opens a STREAM, and the writer renders one
// document: it emits no directive, and the line the author wrote has no
// place in the written form. It does not disappear in silence either — the
// rewrite is refused, naming the directive — which is the half of ADR-102
// §5 the markers themselves left when #2359 made the writer re-emit them.
// A comment written on a directive's line is named with it.
func TestTheDocumentsCanonicalFormRefusesTheStreamDirectiveItCannotEmit(t *testing.T) {
	for name, tc := range map[string]struct{ src, want string }{
		"a %YAML directive":              {"%YAML 1.1\n---\n" + looseDoc, "%YAML 1.1"},
		"a %TAG directive":               {"%TAG !e! tag:example.com,2000:\n---\n" + looseDoc, "%TAG !e! tag:example.com,2000:"},
		"a directive carrying a comment": {"%YAML 1.1 # keep me\n---\n" + looseDoc, "%YAML 1.1 # keep me"},
		"a directive below a comment":    {"# above\n%YAML 1.1\n---\n" + looseDoc, "%YAML 1.1"},
		"two directives":                 {"%YAML 1.1\n%TAG !e! tag:example.com,2000:\n---\n" + looseDoc, "%YAML 1.1; %TAG !e! tag:example.com,2000:"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Document("x.bot.yaml", []byte(tc.src))
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want a refusal naming %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "stream directive") {
				t.Fatalf("the refusal does not say what it is about: %v", err)
			}
		})
	}
}
