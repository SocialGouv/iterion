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

// A comment the writer would move away from what it described is refused:
// after the `dsl:` header, on its line, after an import, after the first
// declaration; the comments that lead the file are kept where they are.
func TestACommentTheWriterWouldMoveIsRefused(t *testing.T) {
	body := "schema v:\n  ok: bool\n\nagent a:\n  model: \"m\"\n  output: v\n\nworkflow w:\n  worktree: none\n  sandbox: none\n  entry: a\n  a -> done\n"
	for name, tc := range map[string]struct {
		src     string
		refused string
	}{
		"leading comments, then the header":   {"## about the file\n## and more\ndsl: 2\n\n" + body, ""},
		"a comment after the header":          {"## about the file\ndsl: 2\n## about schema v\n" + body, "line 3 follows the `dsl:` header or an import (line 2)"},
		"a trailing comment on the header":    {"## about the file\ndsl: 2 ## trailing\n\n" + body, "line 2 follows the `dsl:` header or an import (line 2)"},
		"a comment after an import":           {"import \"lib/s.bot\"\n## after the import\n\n" + body, "line 2 follows the `dsl:` header or an import (line 1)"},
		"a comment before an import":          {"## before the import\nimport \"lib/s.bot\"\n\n" + body, ""},
		"a comment inside a block":            {strings.Replace(body, "  model: \"m\"\n", "  ## the model\n  model: \"m\"\n", 1), "line 5 follows the first declaration (line 1)"},
		"a hash line inside a prompt is text": {"prompt p:\n  # a title, not a comment\n  body\n\n" + strings.Replace(body, "  model: \"m\"\n", "  model: \"m\"\n  system: p\n", 1), ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Bytes("x.bot", []byte(tc.src))
			switch {
			case tc.refused == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.refused != "" && (!errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), tc.refused)):
				t.Fatalf("want a refusal saying %q, got %v", tc.refused, err)
			}
		})
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
