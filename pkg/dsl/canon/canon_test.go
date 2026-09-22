package canon

import (
	"errors"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
)

const loose = "## A header the writer keeps.\n\n\nschema verdict:\n  ok: bool\n\n\n\nagent check:\n  model: \"m\"\n  output: verdict\n\nworkflow w:\n  worktree: none\n  sandbox: none\n  entry: check\n  check -> done\n"

// The canonical form is written on the file's own bytes: a BOM and CRLF
// line endings are kept, the text between them is the writer's.
func TestTheCanonicalFormKeepsTheBOMAndTheLineEndings(t *testing.T) {
	src := "\ufeff" + strings.ReplaceAll(loose, "\n", "\r\n")
	out, err := Bytes("x.bot", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if !strings.HasPrefix(got, "\ufeff## A header the writer keeps.\r\n") {
		t.Fatalf("the BOM or the line endings were lost:\n%q", got[:60])
	}
	if strings.Contains(got, "\n\n\n") || strings.Contains(strings.ReplaceAll(got, "\r\n", "\n"), "\n\n\n") {
		t.Fatalf("the text was not made canonical:\n%s", got)
	}
	if strings.Count(got, "\n") != strings.Count(got, "\r\n") {
		t.Fatalf("a line ending does not follow the file's own:\n%q", got)
	}
	again, err := Bytes("x.bot", out)
	if err != nil || string(again) != got {
		t.Fatalf("not idempotent: %v", err)
	}
	plain, err := Bytes("x.bot", []byte(loose))
	if err != nil || strings.Contains(string(plain), "\r") || strings.HasPrefix(string(plain), "\ufeff") {
		t.Fatalf("an LF file gained a BOM or a CR: %v %q", err, string(plain)[:40])
	}
}

// A comment keeps the place it was written at: the canonical form puts it
// back above the line it led, inside a block as well as between two
// declarations. Until #1282 every one of these was REFUSED, because the
// writer kept only the comments leading the file and would have hoisted
// the rest to the head.
//
// Two places have no declaration to hold them and do move, by name: a
// comment written at the END of the `dsl:` header's own line (the header is
// not a declaration the AST carries), and one written above the file's
// FIRST declaration but separated from it by a blank line — there the
// file's own comments and the first declaration's are written one after the
// other, so a blank line is all that tells them apart, and a comment on the
// far side of it is the head's. Below the first declaration the header of
// the declaration above separates them and no such rule is needed.
func TestACommentKeepsThePlaceItWasWrittenAt(t *testing.T) {
	body := "schema v:\n  ok: bool\n\nagent a:\n  model: \"m\"\n  output: v\n\nworkflow w:\n  worktree: none\n  sandbox: none\n  entry: a\n  a -> done\n"
	for name, tc := range map[string]struct {
		src string
		// want is a fragment the canonical form must contain: the comment
		// with the line it leads, so a hoist to the head reddens.
		want string
	}{
		"leading comments, then the header":        {"## about the file\n## and more\ndsl: 2\n\n" + body, "## about the file\n## and more\n\ndsl: 2\n"},
		"a comment after the header":               {"## about the file\ndsl: 2\n## about schema v\n" + body, "## about schema v\nschema v:\n"},
		"a comment inside a block":                 {strings.Replace(body, "  model: \"m\"\n", "  ## the model\n  model: \"m\"\n", 1), "agent a:\n  ## the model\n  model: \"m\"\n"},
		"a comment above an edge":                  {strings.Replace(body, "  a -> done\n", "  ## the only edge\n  a -> done\n", 1), "  ## the only edge\n  a -> done\n"},
		"a trailing comment on a property":         {strings.Replace(body, "  model: \"m\"\n", "  model: \"m\" ## why this one\n", 1), "  model: \"m\" ## why this one\n"},
		"a comment before an import":               {"## before the import\nimport \"lib/s.bot\"\n\n" + body, "## before the import\n\nimport \"lib/s.bot\"\n"},
		"a hash line inside a prompt is text":      {"prompt p:\n  # a title, not a comment\n  body\n\n" + strings.Replace(body, "  model: \"m\"\n", "  model: \"m\"\n  system: p\n", 1), "prompt p:\n  # a title, not a comment\n  body\n"},
		"a comment glued to the first declaration": {"## about the file\n\n## about schema v\n" + body, "## about the file\n\n## about schema v\nschema v:\n"},
		// The two that move, said as what they become. A comment above
		// the file's FIRST declaration and separated from it by a blank
		// line is the file's head: nothing in the text tells it from one,
		// and the head is where the writer puts it.
		"a comment after an import":        {"import \"lib/s.bot\"\n## after the import\n\n" + body, "## after the import\n\nimport \"lib/s.bot\"\n"},
		"a trailing comment on the header": {"## about the file\ndsl: 2 ## trailing\n\n" + body, "## about the file\n## trailing\n\ndsl: 2\n"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := Bytes("x.bot", []byte(tc.src))
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !strings.Contains(string(out), tc.want) {
				t.Fatalf("the canonical form does not hold %q:\n%s", tc.want, out)
			}
			again, err := Bytes("x.bot", out)
			if err != nil {
				t.Fatalf("the canonical form was refused on the second pass: %v", err)
			}
			if string(again) != string(out) {
				t.Fatalf("not a fixed point — `fmt --check` could never go green on it:\n%s\nbecame\n%s", out, again)
			}
		})
	}
}

// A comment is never dropped, even when the line it named is one the writer
// does not emit: it is written at the end of the block it was in, and the
// second pass leaves it there.
func TestACommentWhoseLineTheWriterDoesNotEmitIsKept(t *testing.T) {
	// `session: fresh` is the default and the writer leaves it out; the
	// comment above it has nothing left to lead.
	src := "schema v:\n  ok: bool\n\nagent a:\n  model: \"m\"\n  output: v\n  ## why the session is fresh\n  session: fresh\n\nworkflow w:\n  worktree: none\n  sandbox: none\n  entry: a\n  a -> done\n"
	out, err := Bytes("x.bot", []byte(src))
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if strings.Contains(string(out), "session: fresh") {
		t.Fatalf("the fixture no longer exercises a line the writer drops:\n%s", out)
	}
	if !strings.Contains(string(out), "## why the session is fresh") {
		t.Fatalf("the comment went with the line it named:\n%s", out)
	}
	again, err := Bytes("x.bot", out)
	if err != nil || string(again) != string(out) {
		t.Fatalf("not a fixed point: %v\n%s", err, again)
	}
}

// A file that does not parse is refused, not written.
func TestAFileThatDoesNotParseIsRefused(t *testing.T) {
	if _, err := Bytes("b.bot", []byte("agent :\n  model\n")); !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "does not parse") {
		t.Fatalf("%v", err)
	}
}

// The text is proven before it is handed back: a document the writer cannot
// carry as the same program — here a prompt body whose first line sits
// deeper than a later one, which no text can hold and the parser never
// produces — is refused.
func TestTheTextIsProvenBeforeItIsHandedBack(t *testing.T) {
	deep := &ast.File{Prompts: []*ast.PromptDecl{{Name: "deep", Body: "    first, deeper\nsecond, shallower"}}}
	if text, err := provenText(deep); err == nil {
		t.Fatalf("a body no text can carry was written:\n%s", text)
	}
	sound := &ast.File{Prompts: []*ast.PromptDecl{{Name: "sound", Body: "first\n  second, deeper"}}}
	if _, err := provenText(sound); err != nil {
		t.Fatalf("a sound body was refused: %v", err)
	}
}
