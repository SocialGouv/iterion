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

// #2650 — the engine pushes the run's resolved ${PROJECT_SCRATCH_DIR} onto
// the executor (SetScratchDir, next to SetWorkDir/SetRepoRoot), and that
// push must widen the VERY registry the host registered the read tools
// against. This exercises the seam the way runview wires it: one ReadScope
// shared between RegisterClawBuiltinsWithScope and WithClawReadScope.
func TestSetScratchDirWidensTheRegisteredReadTools(t *testing.T) {
	t.Parallel()
	workspace := t.TempDir()
	scratch := t.TempDir()
	briefDir := filepath.Join(scratch, "rgaa-audit", "run-01", "briefs")
	if err := os.MkdirAll(briefDir, 0o755); err != nil {
		t.Fatal(err)
	}
	brief := filepath.Join(briefDir, "brief-lot-01.md")
	if err := os.WriteFile(brief, []byte("# Brief\n"), 0o644); err != nil {
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
	call := func() error {
		_, err := read.Execute(context.Background(), []byte(`{"path": "`+brief+`"}`))
		return err
	}

	// The witness: before the engine's push, the brief refuses exactly the
	// way run 01a15f60's nineteen batches saw it.
	if err := call(); err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("before SetScratchDir, read_file(%q) must refuse with 'outside workspace', got %v", brief, err)
	}
	exec.SetScratchDir(scratch)
	if err := call(); err != nil {
		t.Fatalf("after SetScratchDir, read_file(%q) refused: %v", brief, err)
	}
	if got := scope.ExtraRoots(); len(got) != 1 || got[0] != scratch {
		t.Fatalf("ExtraRoots = %v, want [%q]", got, scratch)
	}
}

// Without a shared scope the push is a recorded no-op, not a widening of a
// registry nobody calls.
func TestSetScratchDirWithoutScopeIsANoOp(t *testing.T) {
	t.Parallel()
	exec := NewClawExecutor(NewRegistry(), &ir.Workflow{})
	exec.SetScratchDir(t.TempDir()) // must not panic, must not widen anything
}
