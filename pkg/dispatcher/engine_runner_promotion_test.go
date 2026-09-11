package dispatcher

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/botscaffold"
	"github.com/SocialGouv/iterion/pkg/bundle"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runview"
)

// TestEngineRunnerPromotesBareMainBot: the dispatcher's engine path given
// `<bundle>/main.bot` compiles the BUNDLE — its prompts/*.md in scope, its
// hash — the way `iterion run`, the studio and a resume do, so a run it
// launches resumes elsewhere without `--force`. The fixture is the
// multi-file gallery shape, whose main.bot declares no prompt of its own:
// a bare compile of it fails on the prompt references (C003), so a green
// runner here is the promotion itself.
func TestEngineRunnerPromotesBareMainBot(t *testing.T) {
	tpl, ok := botscaffold.TemplateByID("multi-file")
	if !ok {
		t.Fatal("no multi-file template")
	}
	spec := tpl.Spec
	spec.Slug = "mf"
	// Pinned: the compile refuses C018 on a credential-less host.
	spec.Model, spec.Backend = "anthropic/claude-opus-4-8", "claude_code"
	dir := filepath.Join(t.TempDir(), spec.Slug)
	if _, err := botscaffold.Scaffold(dir, spec); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	b, err := bundle.OpenDir(dir)
	if err != nil {
		t.Fatalf("OpenDir: %v", err)
	}
	_, want, err := runview.CompileBundleWorkflow(b.IterPath, b)
	if err != nil {
		t.Fatalf("CompileBundleWorkflow: %v", err)
	}

	logger := iterlog.New(iterlog.LevelError, &bytes.Buffer{})
	r, err := NewEngineRunner(filepath.Join(dir, "main.bot"), logger)
	if err != nil {
		t.Fatalf("NewEngineRunner on the bare main.bot: %v", err)
	}
	defer r.Close()
	if r.workflowHash != want {
		t.Fatalf("workflowHash = %s, want the bundle's %s", r.workflowHash, want)
	}
	// The promotion is the whole bundle, not the compile alone: without the
	// handle the run would get the prompts and the hash but none of the
	// bundle's skills/ — a silent loss where a bare compile used to refuse.
	if r.bundle == nil || r.bundle.IterPath != b.IterPath {
		t.Fatalf("the promoted run carries bundle %+v, want the bundle at %s", r.bundle, b.IterPath)
	}
	// The config named a FILE: the ADR-046 service path (which promotes the
	// same file itself) stays reachable for it, unlike a bundle directory.
	if r.bundleConfigured {
		t.Fatalf("a promoted main.bot config reads as bundle-configured; the service path would be bypassed")
	}
	dirRunner, err := NewEngineRunner(dir, logger)
	if err != nil {
		t.Fatalf("NewEngineRunner on the bundle dir: %v", err)
	}
	defer dirRunner.Close()
	if !dirRunner.bundleConfigured {
		t.Fatalf("a bundle directory config does not read as bundle-configured")
	}
	// And a .botz archive — the third config kind, whose extracted handle
	// is the one the direct path shares across dispatches.
	archive := filepath.Join(t.TempDir(), "mf.botz")
	if _, err := bundle.PackDir(dir, archive); err != nil {
		t.Fatalf("PackDir: %v", err)
	}
	archiveRunner, err := NewEngineRunner(archive, logger)
	if err != nil {
		t.Fatalf("NewEngineRunner on the archive: %v", err)
	}
	defer archiveRunner.Close()
	if !archiveRunner.bundleConfigured {
		t.Fatalf("a bundle archive config does not read as bundle-configured")
	}
}
