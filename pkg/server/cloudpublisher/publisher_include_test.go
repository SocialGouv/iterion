package cloudpublisher

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The AST that travels to a runner must compile there, where none of the
// files beside the source exist: a prompt's {{include}} is resolved into the
// body on the server, for the main.bot of a bundle launch and for the
// bundle's prompts/*.md alike (#1013).
func TestBundleLaunchInlinesIncludesForTheRunner(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"main.bot":            "schema out:\n  ok: bool\n\nprompt p:\n  Main says:\n  {{include \"part.md\"}}\n\nagent a:\n  model: \"m\"\n  output: out\n  system: p\n\nagent b:\n  model: \"m\"\n  output: out\n  system: contract\n\nworkflow main:\n  entry: a\n  a -> b\n  b -> done\n",
		"part.md":             "PART-FROM-MAIN",
		"prompts/contract.md": "Contract says:\n{{include \"sibling.md\"}}\n",
		"prompts/sibling.md":  "SIBLING-FROM-PROMPTS",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	source, err := os.ReadFile(filepath.Join(dir, "main.bot"))
	if err != nil {
		t.Fatal(err)
	}
	// The client names a path of its own; the snapshot on this server is
	// what the includes resolve against.
	body, err := marshalIRFromSpec("bots/probe/main.bot", string(source), dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PART-FROM-MAIN", "SIBLING-FROM-PROMPTS"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("serialised AST lacks the inlined text %q", want)
		}
	}
	if bytes.Contains(body, []byte("{{include")) {
		t.Error("an include marker still travels")
	}

	// The runner: a pod with none of the files, a different working directory.
	t.Chdir(t.TempDir())
	f, err := ast.UnmarshalFile(body)
	if err != nil {
		t.Fatal(err)
	}
	cr := ir.Compile(f)
	if cr.HasErrors() {
		t.Fatalf("the runner's compile fails: %v", cr.Diagnostics)
	}
	if !strings.Contains(cr.Workflow.Prompts["contract"].Body, "SIBLING-FROM-PROMPTS") {
		t.Errorf("bundle prompt lost its include on the runner: %q", cr.Workflow.Prompts["contract"].Body)
	}
}

// An inline upload has no files beside its source: an include it carries
// is refused at publish, with the remedy, instead of dying on the runner
// with C055 — and never resolved against the server's working directory.
func TestInlineLaunchRefusesAnIncludeItCannotCarry(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "rules.md"), []byte("SERVER-FILE"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	source := "prompt p:\n  {{include \"rules.md\"}}\n\nworkflow main:\n  entry: done\n"
	_, err := marshalIRFromSpec("", source)
	if err == nil || !strings.Contains(err.Error(), "bundle") {
		t.Fatalf("want a refusal naming the bundle remedy, got %v", err)
	}
	_, err = marshalIRFromSpec("bots/probe/main.bot", source)
	if err == nil || !strings.Contains(err.Error(), "bundle") {
		t.Fatalf("a client-named path is not a server path; want the same refusal, got %v", err)
	}
}
