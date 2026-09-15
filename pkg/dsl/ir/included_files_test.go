package ir

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// TestCompileReportsTheFilesItsIncludesRead: the compile names every file
// the prompts' {{include}} markers read — nested ones too, each once,
// sorted — so a caller can fold what the agent reads into the source's
// identity. A file with no include reports none.
func TestCompileReportsTheFilesItsIncludesRead(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("notes/a.md", "Alpha {{include \"b.md\"}}")
	write("notes/b.md", "Beta")
	// Pinned model and backend: the compile refuses C018 on a
	// credential-less host, and the test measures the closure.
	head := "schema out:\n  ok: bool\n\nprompt mission:\n  {{include \"notes/a.md\"}}\n\n"
	tail := "agent worker:\n  backend: \"claude_code\"\n  model: \"anthropic/claude-opus-4-8\"\n  system: mission\n  output: out\n\nworkflow w:\n  entry: worker\n  worker -> done\n"
	compile := func(src string) *CompileResult {
		t.Helper()
		mainBot := filepath.Join(root, "main.bot")
		write("main.bot", src)
		pr := parser.Parse(mainBot, src)
		for _, d := range pr.Diagnostics {
			if d.Severity == parser.SeverityError {
				t.Fatalf("parse error: %s", d.Error())
			}
		}
		cr := Compile(pr.File)
		if cr.HasErrors() {
			t.Fatalf("compile errors: %v", cr.Diagnostics)
		}
		return cr
	}

	want := []string{filepath.Join(root, "notes", "a.md"), filepath.Join(root, "notes", "b.md")}
	if got := compile(head + tail).IncludedFiles; !reflect.DeepEqual(got, want) {
		t.Fatalf("included files %v, want %v", got, want)
	}
	// A second prompt including a file the first already reached reports it once.
	twice := head + "prompt again:\n  {{include \"notes/b.md\"}} and {{include \"notes/a.md\"}}\n\n" + tail
	if got := compile(twice).IncludedFiles; !reflect.DeepEqual(got, want) {
		t.Fatalf("included files with a second prompt %v, want %v once each", got, want)
	}
	plain := "schema out:\n  ok: bool\n\nprompt mission:\n  Do the thing.\n\n" + tail
	if got := compile(plain).IncludedFiles; got != nil {
		t.Fatalf("a file with no include reports %v", got)
	}
}
