package model

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/tool"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// #2650 — the engine pushes the run's scratch read roots onto the
// executor (SetScratchDirs, next to SetWorkDir/SetRepoRoot), and that
// push must widen the VERY registry the host registered the read tools
// against. This exercises the seam the way runview wires it: one
// ReadScope shared between RegisterClawBuiltinsWithScope and
// WithClawReadScope. The roots arrive narrowed (#2869): the run family's
// subtree first, the shared project scratch behind it while the workflow
// still references it.
func TestSetScratchDirsWidensTheRegisteredReadTools(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	scratch := t.TempDir()
	runSubtree := filepath.Join(scratch, "runs", "run-01")
	briefDir := filepath.Join(runSubtree, "rgaa-audit", "briefs")
	if err := os.MkdirAll(briefDir, 0o755); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(briefDir, "brief-lot-01.md")
	if err := os.WriteFile(brief, []byte("# Brief\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A leftover from ANOTHER run of the same project, under the shared
	// root but outside this run's subtree.
	otherDir := filepath.Join(scratch, "runs", "run-00", "briefs")
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(otherDir, "brief-lot-00.md")
	if err := os.WriteFile(other, []byte("# Other\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	scope := tool.NewReadScope(workspace)
	toolReg := tool.NewRegistry()
	if err := tool.RegisterClawBuiltinsWithScope(toolReg, scope, nil); err != nil {
		t.Fatal(err)
	}
	exec := NewClawExecutor(NewRegistry(), &ir.Workflow{},
		WithToolRegistry(toolReg), WithClawReadScope(scope))

	read, err := toolReg.Resolve("read_file")
	if err != nil {
		t.Fatal(err)
	}
	call := func(path string) error {
		_, err := read.Execute(context.Background(), []byte(`{"path": "`+path+`"}`))
		return err
	}

	// The witness: before the engine's push, the brief refuses exactly the
	// way run 01a15f60's nineteen batches saw it.
	if err := call(brief); err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("before SetScratchDirs, read_file(%q) must refuse with 'outside workspace', got %v", brief, err)
	}

	// The narrowed push: the run's subtree alone, no shared root. The
	// run's own brief reads; the other run's leftover refuses (#2869).
	exec.SetScratchDirs(scratch, runSubtree, false)
	if err := call(brief); err != nil {
		t.Fatalf("after SetScratchDirs(base, run, false), read_file(%q) refused: %v", brief, err)
	}
	if err := call(other); err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("with the narrowed push, read_file(%q) — another run's leftover — must refuse, got %v", other, err)
	}
	wantRun := mustCanonicalRoot(t, runSubtree)
	if got := scope.ExtraRoots(); len(got) != 1 || got[0] != wantRun {
		t.Fatalf("ExtraRoots = %v, want the canonical [%q]", got, wantRun)
	}

	// The workflow still referencing ${PROJECT_SCRATCH_DIR}: the shared
	// root rides along, and the leftover reads again — today's posture
	// kept for an unmigrated bot.
	exec.SetScratchDirs(scratch, runSubtree, true)
	if err := call(other); err != nil {
		t.Fatalf("after SetScratchDirs(base, run, true), read_file(%q) refused: %v", other, err)
	}
	wantShared := mustCanonicalRoot(t, scratch)
	if got := scope.ExtraRoots(); len(got) != 2 || got[0] != wantRun || got[1] != wantShared {
		t.Fatalf("ExtraRoots = %v, want the canonical [%q %q]", got, wantRun, wantShared)
	}
	if anchor, roots := exec.scratchScope(); anchor != scratch || len(roots) != 2 || roots[0] != runSubtree || roots[1] != scratch {
		t.Fatalf("scratchScope = %q %v, want the pushed anchor %q and roots [%q %q]", anchor, roots, scratch, runSubtree, scratch)
	}
}

// The push attaches the run subtree BENEATH the project scratch: other
// runs write the shared scratch (sandboxed ones through their bind), so a
// link planted at runs/<id> before the push must contain nothing — not
// become a read root on the link's target.
func TestSetScratchDirsDoesNotAdoptALinkPlantedAtTheRunSubtree(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	scratch := t.TempDir()
	outside := t.TempDir()
	hostFile := filepath.Join(outside, "host-file.txt")
	if err := os.WriteFile(hostFile, []byte("HOST ONLY\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSubtree := filepath.Join(scratch, "runs", "run-planted")
	if err := os.MkdirAll(filepath.Dir(runSubtree), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, runSubtree); err != nil {
		t.Fatal(err)
	}

	scope := tool.NewReadScope(workspace)
	toolReg := tool.NewRegistry()
	if err := tool.RegisterClawBuiltinsWithScope(toolReg, scope, nil); err != nil {
		t.Fatal(err)
	}
	exec := NewClawExecutor(NewRegistry(), &ir.Workflow{}, WithToolRegistry(toolReg), WithClawReadScope(scope))
	exec.SetScratchDirs(scratch, runSubtree, false)

	for _, root := range scope.ExtraRoots() {
		if root == mustCanonicalRoot(t, outside) {
			t.Fatalf("ExtraRoots = %v: the link planted at the run subtree became a read root", scope.ExtraRoots())
		}
	}
	read, err := toolReg.Resolve("read_file")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := read.Execute(context.Background(), []byte(`{"path": "`+hostFile+`"}`)); err == nil {
		t.Fatalf("read_file(%q) = %q through a link planted at the run subtree, want a refusal", hostFile, out)
	}
}

// Without a shared scope the push still records (the sandbox crossing
// narrows from the record) but widens no registry nobody calls.
func TestSetScratchDirsWithoutScopeStillRecords(t *testing.T) {
	t.Parallel()
	exec := NewClawExecutor(NewRegistry(), &ir.Workflow{})
	base := t.TempDir()
	exec.SetScratchDirs(base, filepath.Join(base, "runs", "r"), false) // must not panic
	if anchor, roots := exec.scratchScope(); anchor != base || len(roots) != 1 {
		t.Fatalf("scratchScope = %q %v, want the recorded anchor and run subtree", anchor, roots)
	}
}
