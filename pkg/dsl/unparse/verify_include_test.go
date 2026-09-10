package unparse

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

const includeSource = "schema out:\n  ok: bool\n\nprompt p:\n  Before.\n  {{include \"rules.md\"}}\n  After.\n\nagent a:\n  model: \"m\"\n  output: out\n  system: p\n\nworkflow w:\n  entry: a\n  a -> done\n"

// A document that came through the JSON transport has no source file, so
// the compiler refuses its {{include}} on both sides of the guard alike —
// the guard must accept it: the file written keeps the marker, which the
// next compile resolves beside the file on disk. Verify used to parse the
// round-trip under a made-up name, so ITS side resolved the include against
// the working directory and every bot with an include was refused at save.
func TestVerifyAcceptsAnIncludeFromTheJSONTransport(t *testing.T) {
	pr := parser.Parse("/elsewhere/main.bot", includeSource)
	for _, d := range pr.Diagnostics {
		t.Fatalf("parse: %s", d.Error())
	}
	data, err := ast.MarshalFile(pr.File)
	if err != nil {
		t.Fatal(err)
	}
	f, err := ast.UnmarshalFile(data)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir()) // a working directory with no rules.md, like a server's
	text := Unparse(f)
	if err := Verify(f, text); err != nil {
		t.Fatalf("Verify refused a document with an include: %v", err)
	}
	// The verdict must not depend on what the process sees beside itself.
	if err := os.WriteFile("rules.md", []byte("THE SERVER'S FILE"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Verify(f, text); err != nil {
		t.Fatalf("Verify refused the same document once a rules.md sat in the cwd: %v", err)
	}
	if !strings.Contains(text, `{{include "rules.md"}}`) {
		t.Fatalf("the marker did not survive the round-trip:\n%s", text)
	}
}

// A document parsed from a file resolves its include beside that file on
// both sides, so the guard sees one program whether the include exists or
// not.
func TestVerifyResolvesAnIncludeBesideTheDocumentsFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rules.md"), []byte("RULES"), 0o644); err != nil {
		t.Fatal(err)
	}
	pr := parser.Parse(filepath.Join(dir, "main.bot"), includeSource)
	t.Chdir(t.TempDir())
	if err := Verify(pr.File, Unparse(pr.File)); err != nil {
		t.Fatalf("Verify refused a file whose include resolves beside it: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "rules.md")); err != nil {
		t.Fatal(err)
	}
	if err := Verify(pr.File, Unparse(pr.File)); err != nil {
		t.Fatalf("Verify refused a file whose include is missing on both sides: %v", err)
	}
}
