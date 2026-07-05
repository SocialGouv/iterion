package ir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// botWithInclude builds a minimal, compilable .bot source whose `sys`
// prompt embeds an {{include "..."}} marker for the given relative path.
func botWithInclude(includePath string) string {
	return `
schema empty:
  ok: bool

prompt sys:
  Base instructions.
  {{include "` + includePath + `"}}
  End instructions.

prompt usr:
  Do something.

agent start:
  model: "test-model"
  input: empty
  output: empty
  system: sys
  user: usr

workflow minimal:
  entry: start
  start -> done
`
}

// compileAt parses/compiles src as though it lived at botPath, so the
// prompt-include base directory resolves to filepath.Dir(botPath).
func compileAt(t *testing.T, botPath, src string) *CompileResult {
	t.Helper()
	res := parser.Parse(botPath, src)
	for _, d := range res.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("parse error: %s", d.Error())
		}
	}
	return Compile(res.File)
}

func TestCompilePromptInclude_InlinesFileContent(t *testing.T) {
	dir := t.TempDir()
	const rules = "RULE ONE: be terse.\nRULE TWO: cite sources."
	if err := os.WriteFile(filepath.Join(dir, "rules.md"), []byte(rules), 0o600); err != nil {
		t.Fatal(err)
	}

	cr := compileAt(t, filepath.Join(dir, "bot.bot"), botWithInclude("rules.md"))
	if cr.HasErrors() {
		t.Fatalf("unexpected compile errors: %v", cr.Diagnostics)
	}
	p, ok := cr.Workflow.Prompts["sys"]
	if !ok {
		t.Fatal("prompt 'sys' not found")
	}
	if !strings.Contains(p.Body, rules) {
		t.Fatalf("include content not inlined into prompt body:\n%s", p.Body)
	}
	// The surrounding prompt text must survive around the injection.
	if !strings.Contains(p.Body, "Base instructions.") || !strings.Contains(p.Body, "End instructions.") {
		t.Fatalf("surrounding prompt text lost: %q", p.Body)
	}
	// The marker itself must be gone.
	if strings.Contains(p.Body, "{{include") {
		t.Fatalf("include marker left unexpanded: %q", p.Body)
	}
}

func TestCompilePromptInclude_MissingFile(t *testing.T) {
	dir := t.TempDir()
	cr := compileAt(t, filepath.Join(dir, "bot.bot"), botWithInclude("does-not-exist.md"))
	if !hasDiag(cr.Diagnostics, DiagBadPromptInclude) {
		t.Fatalf("expected %s for missing include file, got: %v", DiagBadPromptInclude, cr.Diagnostics)
	}
}

func TestReadPromptInclude_RejectsUnsafePaths(t *testing.T) {
	dir := t.TempDir()
	// A real file OUTSIDE the base dir the escape attempt would reach.
	outside := filepath.Join(filepath.Dir(dir), "secret.txt")
	if err := os.WriteFile(outside, []byte("top secret"), 0o600); err == nil {
		defer os.Remove(outside)
	}

	cases := []struct {
		name string
		rel  string
	}{
		{"escape", "../secret.txt"},
		{"absolute", outside},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := readPromptInclude(dir, tc.rel); err == nil {
				t.Fatalf("expected error for %q, got nil", tc.rel)
			}
		})
	}
}

func TestReadPromptInclude_SizeCap(t *testing.T) {
	dir := t.TempDir()
	big := make([]byte, maxPromptIncludeBytes+1)
	if err := os.WriteFile(filepath.Join(dir, "big.md"), big, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPromptInclude(dir, "big.md"); err == nil {
		t.Fatal("expected size-cap error, got nil")
	}
}
