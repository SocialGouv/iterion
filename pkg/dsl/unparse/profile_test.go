package unparse_test

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unparse"
)

// A profile-2 document is written back as a profile-2 file: the header on
// the first significant line after the head comments, standard escapes in
// every string, no directive — and the text reads back in profile 2 with
// the same values.
func TestUnparseWritesTheProfileTwoHeader(t *testing.T) {
	src := "## ---\n## name: probe\n## ---\n## a note\ndsl: 2\n\ntool t:\n  command: \"a\\nb \\\"q\\\"\"\n\nworkflow w:\n  entry: t\n  t -> done\n"
	res := parser.Parse("x.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("fixture does not parse: %v", res.Diagnostics)
	}
	text := unparse.Unparse(res.File)
	want := "## ---\n## name: probe\n## ---\n## a note\n\ndsl: 2\n\n"
	if !strings.HasPrefix(text, want) {
		t.Fatalf("head is not frontmatter, comments, header:\n%s", text)
	}
	if strings.Contains(text, "strict-escape") {
		t.Fatalf("a profile-2 file carries no directive:\n%s", text)
	}
	if pre := parser.ReadPreamble(text); pre.Profile != 2 {
		t.Fatalf("the written text reads as profile %d:\n%s", pre.Profile, text)
	}
	back := parser.Parse("x.bot", text)
	if len(back.Diagnostics) != 0 {
		t.Fatalf("the written text does not parse: %v\n%s", back.Diagnostics, text)
	}
	if got := back.File.Tools[0].Command; got != "a\nb \"q\"" {
		t.Fatalf("command read back as %q", got)
	}
	if err := unparse.Verify(res.File, text); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// A profile-1 file written strict keeps its catalogue identity: the
// directive goes AFTER the frontmatter block, whose fence must stay on the
// first non-blank line, and still within the lexer's 32-line window.
func TestUnparseWritesTheDirectiveAfterTheFrontmatter(t *testing.T) {
	f := &ast.File{
		Comments: []*ast.Comment{{Text: "---"}, {Text: "name: probe"}, {Text: "---"}, {Text: "a note"}},
		Tools:    []*ast.ToolNodeDecl{{Name: "t", Command: "grep `x` \"y\""}},
	}
	text := unparse.Unparse(f)
	want := "## ---\n## name: probe\n## ---\n## strict-escape: on\n## a note\n"
	if !strings.HasPrefix(text, want) {
		t.Fatalf("head:\n%s", text)
	}
	if pre := parser.ReadPreamble(text); !pre.StrictEscape || pre.Profile != 0 {
		t.Fatalf("the written text does not read strict profile 1: %+v\n%s", pre, text)
	}
	back := parser.Parse("x.bot", text)
	if got := back.File.Tools[0].Command; got != "grep `x` \"y\"" {
		t.Fatalf("command read back as %q", got)
	}
}

// The profile is not program — the same declarations compile the same in
// either — so the save guard checks it by itself: a text that reads in
// another profile than the document's is refused.
func TestVerifyRefusesAProfileMismatch(t *testing.T) {
	src := "dsl: 2\n\ntool t:\n  command: \"x\"\n\nworkflow w:\n  entry: t\n  t -> done\n"
	res := parser.Parse("x.bot", src)
	if len(res.Diagnostics) != 0 {
		t.Fatalf("fixture: %v", res.Diagnostics)
	}
	lost := strings.TrimPrefix(src, "dsl: 2\n")
	err := unparse.Verify(res.File, lost)
	if err == nil || !strings.Contains(err.Error(), "reads as dsl profile 1, the document is profile 2") {
		t.Fatalf("Verify = %v, want the profile mismatch", err)
	}
	// And the other way round.
	v1 := parser.Parse("x.bot", lost)
	if err := unparse.Verify(v1.File, src); err == nil || !strings.Contains(err.Error(), "reads as dsl profile 2, the document is profile 1") {
		t.Fatalf("Verify = %v, want the profile mismatch", err)
	}
}
