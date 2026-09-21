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
// resolves a relative workflow path to an absolute one at the mount
// chokepoint — the docker `--mount source=…` argument refuses a
// non-absolute path. WithFilePath itself stores the launcher's meaning
// verbatim; the absolutisation lives ONLY where the mount is built
// (`bundleResourceDir` in `pkg/runtime/sandbox_devbox.go`), so
// readers of `Run.FilePath` (`pkg/runview/workflow_path.go`,
// `pkg/server/run_delegation.go`, the dispatcher) see the shape the
// launcher wrote. Without the mount-side absolutisation, `iterion
// resume --file examples/foo.bot` on a docker host dies at sandbox
// start on the operator-facing "mount path must be absolute" error —
// the second facet of #1435.
//
// Mutation: remove the filepath.Abs branch from bundleResourceDir →
// the relative sub-case reddens with the returned dir still relative.
func TestBundleResourceDir_AbsolutisesRelative(t *testing.T) {
	// A relative workflow path resolves to an absolute directory.
	dir := bundleResourceDir(nil, "examples/foo.bot")
	if !filepath.IsAbs(dir) {
		t.Fatalf("bundleResourceDir(examples/foo.bot) = %q, want an absolute path — the docker bind-mount source refuses a relative one (#1435)", dir)
	}

	// An already-absolute path passes through unchanged (as its parent).
	abs := filepath.Join(t.TempDir(), "workflow.bot")
	if got := bundleResourceDir(nil, abs); got != filepath.Dir(abs) {
		t.Fatalf("bundleResourceDir(%q) = %q, want %q — an already-absolute parent must not be rewritten", abs, got, filepath.Dir(abs))
	}

	// Empty workflowPath returns empty (no bundle either).
	if got := bundleResourceDir(nil, ""); got != "" {
		t.Fatalf("bundleResourceDir(\"\") = %q, want empty", got)
	}

	// WithFilePath keeps the persisted value verbatim so readers of
	// Run.FilePath see the launcher's meaning.
	eng := &Engine{}
	WithFilePath("examples/foo.bot")(eng)
	if eng.filePath != "examples/foo.bot" {
		t.Fatalf("WithFilePath stored %q, want the verbatim relative input — Run.FilePath readers key on the launcher's meaning", eng.filePath)
	}
}
