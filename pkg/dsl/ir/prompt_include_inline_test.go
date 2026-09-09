package ir

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// InlinePromptIncludes makes an AST self-contained: the markers are
// replaced by the files' content, resolved beside each prompt's own source
// file, so the file compiles anywhere the files are not.
func TestInlinePromptIncludes_MakesTheASTSelfContained(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "rules.md"), []byte("RULES-TEXT"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "schema out:\n  ok: bool\n\nprompt p:\n  Before.\n  {{include \"rules.md\"}}\n  After.\n\nagent a:\n  model: \"m\"\n  output: out\n  system: p\n\nworkflow w:\n  entry: a\n  a -> done\n"
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	pr := parser.Parse(filepath.Join(dir, "main.bot"), src)
	for _, d := range pr.Diagnostics {
		t.Fatalf("parse: %s", d.Error())
	}
	if err := InlinePromptIncludes(pr.File); err != nil {
		t.Fatal(err)
	}
	body := pr.File.Prompts[0].Body
	if !strings.Contains(body, "RULES-TEXT") || HasPromptInclude(body) {
		t.Fatalf("include not inlined: %q", body)
	}
	// Compiled from a directory that has no rules.md, the prompt is whole.
	t.Chdir(t.TempDir())
	cr := Compile(pr.File)
	if cr.HasErrors() {
		t.Fatalf("compile away from the files: %v", cr.Diagnostics)
	}
	if !strings.Contains(cr.Workflow.Prompts["p"].Body, "RULES-TEXT") {
		t.Errorf("compiled prompt lost the inlined text: %q", cr.Workflow.Prompts["p"].Body)
	}
}

// A prompt whose source file is not on this host (an inline upload) cannot
// resolve an include — never against the process working directory, even
// when a file of that name sits there.
func TestInlinePromptIncludes_RefusesAPromptWhoseSourceIsNotOnThisHost(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "leak.md"), []byte("SERVER-FILE"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	pr := parser.Parse("<inline>", "prompt p:\n  {{include \"leak.md\"}}\n")
	err := InlinePromptIncludes(pr.File)
	if err == nil || !strings.Contains(err.Error(), "not on this host") {
		t.Fatalf("want a refusal naming the absent source file, got %v", err)
	}
	if strings.Contains(pr.File.Prompts[0].Body, "SERVER-FILE") {
		t.Fatal("the working directory's file was read")
	}
}

// A missing include is an error naming the prompt, and the body is left as
// written (never replaced by an empty string).
func TestInlinePromptIncludes_ReportsAMissingFile(t *testing.T) {
	dir := t.TempDir()
	src := "prompt p:\n  {{include \"absent.md\"}}\n"
	if err := os.WriteFile(filepath.Join(dir, "main.bot"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	pr := parser.Parse(filepath.Join(dir, "main.bot"), src)
	err := InlinePromptIncludes(pr.File)
	if err == nil || !strings.Contains(err.Error(), `prompt "p"`) || !strings.Contains(err.Error(), "absent.md") {
		t.Fatalf("want an error naming the prompt and the file, got %v", err)
	}
	if !HasPromptInclude(pr.File.Prompts[0].Body) {
		t.Fatal("the marker was dropped although the include failed")
	}
}
