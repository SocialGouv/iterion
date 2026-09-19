package runtime

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestRunResolveDoc_PersistsLaunchSandboxAndMergeOverrides asserts that
// the four launch-time overrides #1435 and #1366 name — sandbox mode,
// sandbox default image, sandbox host_state, and worktree
// merge_into/branch_name — land on the run record's dedicated fields at
// launch. Without persistence, a resume rebuilt from scratch (the CLI
// path, the studio's `Service.Resume`, an unattended usage-window
// retry) recomputes them from env + workflow and silently changes the
// isolation or merge decision the operator took at launch.
//
// Mutation: drop any of the guarded writers at engine_run.go:454-467 →
// the corresponding sub-assertion here reddens with the field left at
// its zero value.
func TestRunResolveDoc_PersistsLaunchSandboxAndMergeOverrides(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-launch-override-persist"
	wf := &ir.Workflow{
		Name:  "launch_overrides",
		Entry: "done",
		Nodes: map[string]ir.Node{"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}},
	}
	eng := New(wf, s, newStubExecutor(),
		WithSandboxOverride("none"),
		WithSandboxDefaultImage("ghcr.io/example/img:v42"),
		WithSandboxHostStateOverride("none"),
		WithMergeInto("none"),
		WithBranchName("feat/keep-me"),
	)
	if err := eng.Run(ctx, runID, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got.SandboxOverride != "none" {
		t.Errorf("SandboxOverride=%q, want %q — --sandbox is silently lost across resume without this", got.SandboxOverride, "none")
	}
	if got.SandboxDefaultImage != "ghcr.io/example/img:v42" {
		t.Errorf("SandboxDefaultImage=%q, want the launched image", got.SandboxDefaultImage)
	}
	if got.SandboxHostState != "none" {
		t.Errorf("SandboxHostState=%q, want %q", got.SandboxHostState, "none")
	}
	if got.MergeInto != "none" {
		t.Errorf("MergeInto=%q, want %q — --merge-into none is silently lost across resume without this", got.MergeInto, "none")
	}
	if got.BranchName != "feat/keep-me" {
		t.Errorf("BranchName=%q, want %q", got.BranchName, "feat/keep-me")
	}
}

// TestWithFilePath_AbsolutisesRelative asserts the runtime option
// resolves a relative workflow path to an absolute one at the
// chokepoint, so downstream consumers (sandbox bind-mount source
// derived from filepath.Dir(e.filePath), the run record's `file_path`)
// never see the relative form. Without this, `iterion resume --file
// examples/foo.bot` on a docker host dies at sandbox start on the
// operator-facing "mount path must be absolute" error — the second
// facet of #1435. Empty stays empty by contract.
//
// Mutation: remove the filepath.Abs branch from WithFilePath → this
// test reddens with e.filePath still equal to the relative input.
func TestWithFilePath_AbsolutisesRelative(t *testing.T) {
	// A relative path must land as absolute on the engine.
	eng := &Engine{}
	WithFilePath("examples/foo.bot")(eng)
	if !filepath.IsAbs(eng.filePath) {
		t.Fatalf("e.filePath = %q, want an absolute path — WithFilePath must absolutise its input", eng.filePath)
	}

	// An empty input stays empty.
	eng2 := &Engine{}
	WithFilePath("")(eng2)
	if eng2.filePath != "" {
		t.Fatalf("e.filePath = %q, want empty — the option is documented as no-op on empty", eng2.filePath)
	}

	// An already-absolute path passes through unchanged.
	abs := filepath.Join(t.TempDir(), "workflow.bot")
	eng3 := &Engine{}
	WithFilePath(abs)(eng3)
	if eng3.filePath != abs {
		t.Fatalf("e.filePath = %q, want %q unchanged — an already-absolute path must not be rewritten", eng3.filePath, abs)
	}
}
