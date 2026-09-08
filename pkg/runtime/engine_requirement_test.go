package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The engine is where every NON-cloud launch surface converges — `iterion
// run`, the studio, the dispatcher's direct path, a subbot child. One gate
// here is what keeps them from each needing their own copy of the arithmetic.

func requireBundle(t *testing.T, requires string) *bundle.Bundle {
	t.Helper()
	dir := t.TempDir()
	manifest := "name: probe\nversion: 1.6.0\n"
	if requires != "" {
		manifest += "requires:\n  iterion: \"" + requires + "\"\n"
	}
	if err := os.WriteFile(filepath.Join(dir, bundle.MainBotFile), []byte("workflow main:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, bundle.ManifestFile), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := bundle.OpenDir(dir)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	return b
}

// pinEngineBuild pins the build the gate compares against. `go test` binaries
// carry appinfo.Version = "dev", which is deliberately unorderable, so the
// comparison would otherwise never run under test.
func pinEngineBuild(t *testing.T, v string) {
	t.Helper()
	prev := engineBuild
	engineBuild = func() string { return v }
	t.Cleanup(func() { engineBuild = prev })
}

func requirementEngine(t *testing.T, requires string) (*Engine, store.RunStore) {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	wf := &ir.Workflow{Name: "main", Entry: "done", Nodes: map[string]ir.Node{
		"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
	}}
	return New(wf, st, nil, WithBundle(requireBundle(t, requires)), WithWorkDir(t.TempDir())), st
}

// A run whose bundle names a newer engine never starts: the gate fires before
// a worktree or a sandbox is spun up for a doomed run, and the run is
// TERMINAL with the typed code.
func TestEngineRun_RefusesABundleRequiringANewerEngine(t *testing.T) {
	pinEngineBuild(t, "v3.112.7+abc123def456")
	e, st := requirementEngine(t, ">= 3.112.14")
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, "run-eng", "main", nil); err != nil {
		t.Fatal(err)
	}
	err := e.Run(ctx, "run-eng", nil)
	if err == nil {
		t.Fatal("Run accepted a bundle this build cannot run")
	}
	var rt *RuntimeError
	if !errors.As(err, &rt) || rt.Code != ErrCodeBotRequiresNewerEngine {
		t.Fatalf("err = %v, want a RuntimeError coded BOT_REQUIRES_NEWER_ENGINE", err)
	}
	if !strings.Contains(err.Error(), "3.112.14") {
		t.Errorf("err = %q, want the floor named", err)
	}
	run, _ := st.LoadRun(ctx, "run-eng")
	if run.Status != store.RunStatusFailed {
		t.Fatalf("status = %q, want failed — nothing ran, and re-running the same pair reaches the same verdict", run.Status)
	}
	if run.FailureCode != store.FailureBotRequiresNewerEngine {
		t.Fatalf("failure code = %q, want BOT_REQUIRES_NEWER_ENGINE", run.FailureCode)
	}
}

// A resume is refused the same way, and BEFORE the claim: the run keeps the
// status it had, so nothing is lost by asking.
func TestEngineResume_RefusesABundleRequiringANewerEngine(t *testing.T) {
	pinEngineBuild(t, "v3.112.7+abc123def456")
	e, st := requirementEngine(t, ">= 3.112.14")
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, "run-eng-r", "main", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.FailRunResumable(ctx, "run-eng-r", &store.Checkpoint{NodeID: "done"}, "earlier", store.FailureExecutionFailed); err != nil {
		t.Fatal(err)
	}
	err := e.Resume(ctx, "run-eng-r", nil)
	if err == nil {
		t.Fatal("Resume accepted a bundle this build cannot run")
	}
	var rt *RuntimeError
	if !errors.As(err, &rt) || rt.Code != ErrCodeBotRequiresNewerEngine {
		t.Fatalf("err = %v, want a RuntimeError coded BOT_REQUIRES_NEWER_ENGINE", err)
	}
	run, _ := st.LoadRun(ctx, "run-eng-r")
	if run.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %q, want the resumable status untouched — the resume was refused before the claim", run.Status)
	}
}

// The negative case: a bundle this build can run, and one that declares
// nothing, both start normally.
func TestEngineRun_AdmitsWhatThisBuildCanRun(t *testing.T) {
	pinEngineBuild(t, "v3.116.4+deadbeef")
	for _, requires := range []string{">= 3.112.14", ""} {
		e, st := requirementEngine(t, requires)
		ctx := context.Background()
		if _, err := st.CreateRun(ctx, "run-ok", "main", nil); err != nil {
			t.Fatal(err)
		}
		if err := e.Run(ctx, "run-ok", nil); err != nil {
			t.Fatalf("Run with requires %q failed: %v", requires, err)
		}
		run, _ := st.LoadRun(ctx, "run-ok")
		if run.Status != store.RunStatusFinished {
			t.Fatalf("status = %q with requires %q, want finished", run.Status, requires)
		}
	}
}
