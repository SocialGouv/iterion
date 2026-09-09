package runview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

func TestMergeBundlePrompts_InjectsNewPrompts(t *testing.T) {
	dir := t.TempDir()
	promptsDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"helper.md":   "Helper body.\n",
		"reviewer.md": "Reviewer body.\n",
		"skip.txt":    "Not markdown — should be ignored.\n",
	} {
		if err := os.WriteFile(filepath.Join(promptsDir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	f := &ast.File{}
	b := &bundle.Bundle{PromptsDir: promptsDir}
	if err := MergeBundlePrompts(f, b); err != nil {
		t.Fatalf("merge: %v", err)
	}
	names := make(map[string]string, len(f.Prompts))
	for _, p := range f.Prompts {
		names[p.Name] = p.Body
	}
	if _, ok := names["helper"]; !ok {
		t.Error("helper prompt missing")
	}
	if _, ok := names["reviewer"]; !ok {
		t.Error("reviewer prompt missing")
	}
	if _, ok := names["skip"]; ok {
		t.Error("non-markdown file was added")
	}
	if got := names["helper"]; got != "Helper body.\n" {
		t.Errorf("helper.Body = %q", got)
	}
}

func TestMergeBundlePrompts_WorkflowDeclaredWinsOnCollision(t *testing.T) {
	dir := t.TempDir()
	promptsDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(promptsDir, "helper.md"), []byte("Bundle body."), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &ast.File{
		Prompts: []*ast.PromptDecl{
			{Name: "helper", Body: "Workflow body."},
		},
	}
	if err := MergeBundlePrompts(f, &bundle.Bundle{PromptsDir: promptsDir}); err != nil {
		t.Fatal(err)
	}
	if got := f.Prompts[0].Body; got != "Workflow body." {
		t.Errorf("workflow body should win on collision, got %q", got)
	}
	if len(f.Prompts) != 1 {
		t.Errorf("expected 1 prompt (workflow's), got %d", len(f.Prompts))
	}
}

// A merged prompt's origin is its markdown file, so a diagnostic on a
// reference inside the body points THERE — not at the main.bot line of the
// node that consumes it, which contains no reference and used to be what the
// position fell back to.
func TestMergeBundlePrompts_DiagnosticsPointAtThePromptFile(t *testing.T) {
	dir := t.TempDir()
	promptsDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mdPath := filepath.Join(promptsDir, "review.md")
	if err := os.WriteFile(mdPath, []byte("Review it.\n\nUses {{vars.typo_var}}.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "schema out:\n  ok: bool\n\nagent reviewer:\n  model: \"m\"\n  output: out\n  system: review\n\nworkflow w:\n  entry: reviewer\n  reviewer -> done\n"
	pr := parser.Parse(filepath.Join(dir, "main.bot"), src)
	for _, d := range pr.Diagnostics {
		t.Fatalf("unexpected parse diagnostic: %s", d.Error())
	}
	if err := MergeBundlePrompts(pr.File, &bundle.Bundle{PromptsDir: promptsDir}); err != nil {
		t.Fatal(err)
	}
	res := ir.Compile(pr.File)
	var found bool
	for _, d := range res.Diagnostics {
		if d.Code != ir.DiagUndeclaredVar {
			continue
		}
		found = true
		if d.File != mdPath || d.Line != 1 {
			t.Errorf("C033 at %s:%d, want %s:1 (the prompt file), got %+v", d.File, d.Line, mdPath, d)
		}
	}
	if !found {
		t.Fatalf("expected a C033 on the undeclared var, got %v", res.Diagnostics)
	}
}

// An `{{include}}` inside a bundle prompt resolves next to THAT file —
// inside prompts/ — and never against the process working directory, which
// on a server is nobody's: a merged prompt used to carry no span, so the
// include base fell back to "." and slurped whatever the cwd held.
func TestMergeBundlePrompts_IncludesResolveNextToThePromptFile(t *testing.T) {
	dir := t.TempDir()
	promptsDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(promptsDir, "helper.md"), []byte("Helper.\n{{include \"sibling.md\"}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(promptsDir, "sibling.md"), []byte("FROM-PROMPTS-DIR"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A decoy in the working directory must not be what the include reads.
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "sibling.md"), []byte("FROM-CWD"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)

	src := "schema out:\n  ok: bool\n\nagent a:\n  model: \"m\"\n  output: out\n  system: helper\n\nworkflow w:\n  entry: a\n  a -> done\n"
	pr := parser.Parse(filepath.Join(dir, "main.bot"), src)
	if err := MergeBundlePrompts(pr.File, &bundle.Bundle{PromptsDir: promptsDir}); err != nil {
		t.Fatal(err)
	}
	res := ir.Compile(pr.File)
	for _, d := range res.Diagnostics {
		if d.Severity == ir.SeverityError {
			t.Fatalf("unexpected compile error: %s", d.Error())
		}
	}
	body := res.Workflow.Prompts["helper"].Body
	if !strings.Contains(body, "FROM-PROMPTS-DIR") || strings.Contains(body, "FROM-CWD") {
		t.Errorf("include resolved against the wrong directory: body = %q", body)
	}

	// With the sibling only in the cwd, the include fails loudly rather
	// than reading the cwd.
	if err := os.Remove(filepath.Join(promptsDir, "sibling.md")); err != nil {
		t.Fatal(err)
	}
	pr = parser.Parse(filepath.Join(dir, "main.bot"), src)
	if err := MergeBundlePrompts(pr.File, &bundle.Bundle{PromptsDir: promptsDir}); err != nil {
		t.Fatal(err)
	}
	res = ir.Compile(pr.File)
	var refused bool
	for _, d := range res.Diagnostics {
		if d.Code == ir.DiagBadPromptInclude {
			refused = true
		}
	}
	if !refused {
		t.Errorf("a sibling present only in the cwd was accepted: %v", res.Diagnostics)
	}
}

func TestMergeBundlePrompts_NilBundleIsNoop(t *testing.T) {
	f := &ast.File{}
	if err := MergeBundlePrompts(f, nil); err != nil {
		t.Errorf("nil bundle should be a no-op, got %v", err)
	}
}

func TestMergeBundlePrompts_NoPromptsDirIsNoop(t *testing.T) {
	f := &ast.File{}
	if err := MergeBundlePrompts(f, &bundle.Bundle{}); err != nil {
		t.Errorf("bundle without prompts dir should be a no-op, got %v", err)
	}
}
