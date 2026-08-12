package cli

import (
	"context"
	"io"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

type subbotLineageExecutor struct{}

func (*subbotLineageExecutor) Execute(_ context.Context, node ir.Node, _ map[string]any) (map[string]any, error) {
	if node.NodeID() == "leaf" {
		return map[string]any{"value": "nested-ok"}, nil
	}
	return map[string]any{}, nil
}

func TestRunSubbotsPersistNestedLineage(t *testing.T) {
	// This file is in package `cli`, so it cannot reach the external
	// test package's hermeticSandbox helper — same reason, stated here:
	// RunRun defaults a sandbox-less bot to `auto`, which needs a
	// container runtime the CI job and the bots' own sandbox lack.
	t.Setenv("ITERION_SANDBOX_DEFAULT", "none")
	dir := t.TempDir()
	rootPath := filepath.Join(dir, "root.bot")
	writeFile(t, rootPath, `schema lineage_result:
  value: string

subbot child:
  source: "child.bot"
  output: lineage_result

workflow root:
  worktree: none
  entry: child
  child -> done
`)
	writeFile(t, filepath.Join(dir, "child.bot"), `schema lineage_result:
  value: string

subbot grandchild:
  source: "grandchild.bot"
  output: lineage_result

workflow child:
  worktree: none
  entry: grandchild
  grandchild -> done
`)
	writeFile(t, filepath.Join(dir, "grandchild.bot"), `schema lineage_result:
  value: string

tool leaf:
  command: "echo nested-ok"
  output: lineage_result

workflow grandchild:
  worktree: none
  entry: leaf
  leaf -> done
`)

	storeDir := filepath.Join(dir, "store")
	err := RunRun(context.Background(), RunOptions{
		File:     rootPath,
		StoreDir: storeDir,
		RunID:    "run-root",
		Executor: &subbotLineageExecutor{},
	}, &Printer{W: io.Discard, Format: OutputJSON})
	if err != nil {
		t.Fatalf("RunRun: %v", err)
	}

	s, err := store.New(storeDir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	childIDs, err := s.ListChildRuns(context.Background(), "run-root")
	if err != nil {
		t.Fatalf("ListChildRuns(root): %v", err)
	}
	if len(childIDs) != 1 {
		t.Fatalf("root children = %v, want exactly one", childIDs)
	}
	child, err := s.LoadRun(context.Background(), childIDs[0])
	if err != nil {
		t.Fatalf("LoadRun(child): %v", err)
	}
	if child.ParentRunID != "run-root" {
		t.Fatalf("child.ParentRunID = %q, want run-root", child.ParentRunID)
	}

	grandchildIDs, err := s.ListChildRuns(context.Background(), child.ID)
	if err != nil {
		t.Fatalf("ListChildRuns(child): %v", err)
	}
	if len(grandchildIDs) != 1 {
		t.Fatalf("child children = %v, want exactly one", grandchildIDs)
	}
	grandchild, err := s.LoadRun(context.Background(), grandchildIDs[0])
	if err != nil {
		t.Fatalf("LoadRun(grandchild): %v", err)
	}
	if grandchild.ParentRunID != child.ID {
		t.Fatalf("grandchild.ParentRunID = %q, want %q", grandchild.ParentRunID, child.ID)
	}
	if child.Status != store.RunStatusFinished || grandchild.Status != store.RunStatusFinished {
		t.Fatalf("nested statuses = child:%q grandchild:%q, want finished", child.Status, grandchild.Status)
	}
}
