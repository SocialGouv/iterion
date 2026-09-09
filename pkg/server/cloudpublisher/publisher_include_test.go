package cloudpublisher

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
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

// An include inside an included file is resolved too, so no marker reaches
// the runner — where it would be refused (C055), or, before the compiler's
// own guard, resolved against the pod's working directory.
func TestBundleLaunchInlinesNestedIncludes(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"main.bot":  "prompt p:\n  {{include \"rules.md\"}}\n\nworkflow main:\n  entry: done\n",
		"rules.md":  "RULES\n{{include \"secret.md\"}}\n",
		"secret.md": "THE BOT'S OWN SECRET",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	source, _ := os.ReadFile(filepath.Join(dir, "main.bot"))
	body, err := marshalIRFromSpec("bots/probe/main.bot", string(source), dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte("THE BOT'S OWN SECRET")) || bytes.Contains(body, []byte("{{include")) {
		t.Fatalf("nested include did not travel resolved: %s", body)
	}
	// A pod whose working directory holds a decoy of the same name.
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "secret.md"), []byte("THE POD'S OWN FILE"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	f, err := ast.UnmarshalFile(body)
	if err != nil {
		t.Fatal(err)
	}
	cr := ir.Compile(f)
	if cr.HasErrors() {
		t.Fatalf("the runner's compile fails: %v", cr.Diagnostics)
	}
	if strings.Contains(cr.Workflow.Prompts["p"].Body, "THE POD'S OWN FILE") {
		t.Fatal("the runner read its own working directory into the prompt")
	}
}

// The entry a bundle launch parses as is the snapshot's own main.bot, which
// bundle.Snapshot.Validate guarantees is there — never a name taken from the
// spec's file path. That path is the CLIENT's word: on a resume it comes
// straight off the request body, while the bundle dir was built by this
// server, so a name derived from it can point at a file the snapshot does
// not hold and refuse a launch whose includes sit right beside the entry.
func TestBundleLaunchParsesTheSnapshotEntryNotTheClientPath(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"main.bot": "prompt p:\n  {{include \"rules.md\"}}\n\nworkflow main:\n  entry: done\n",
		"rules.md": "RULES-FROM-THE-SNAPSHOT",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	source, err := os.ReadFile(filepath.Join(dir, "main.bot"))
	if err != nil {
		t.Fatal(err)
	}
	// A path whose base names no file of the snapshot: taking the entry from
	// it would stat a file that is not there and refuse the publish.
	body, err := marshalIRFromSpec("bots/probe/not-the-entry.bot", string(source), dir)
	if err != nil {
		t.Fatalf("a client path that names another file refused the launch: %v", err)
	}
	if !bytes.Contains(body, []byte("RULES-FROM-THE-SNAPSHOT")) {
		t.Errorf("the include did not resolve beside the snapshot entry: %s", body)
	}
}

// The invariant the entry name above rests on, pinned here because it is
// enforced in another package: a materialised snapshot has main.bot at its
// root — a collection without one never gets that far. Should the snapshot
// format ever admit another entry name, this fails here, at the site that
// reads it, instead of at a publish.
func TestSnapshotAlwaysMaterialisesARootMainBot(t *testing.T) {
	snap := &bundle.Snapshot{Files: map[string]bundle.SnapshotFile{
		"probe/other.bot": {Content: []byte("workflow main:\n  entry: done\n")},
	}, Root: "probe"}
	if err := snap.Validate(); err == nil {
		t.Fatal("a collection whose entry is not main.bot was accepted")
	}
	snap.Files["probe/main.bot"] = bundle.SnapshotFile{Content: []byte("workflow main:\n  entry: done\n")}
	dir, cleanup, err := snap.Materialize()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err := os.Stat(filepath.Join(dir, "main.bot")); err != nil {
		t.Fatalf("a materialised snapshot has no root main.bot: %v", err)
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
