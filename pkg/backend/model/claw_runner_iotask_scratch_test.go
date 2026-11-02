package model

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// The sandbox crossing half of #2869: runnerIOTask narrows the
// in-container runner's scratch read roots to the ones the engine pushed
// (Task.ScratchReadRoots — the run family's subtree first, the shared
// project scratch behind it while still referenced). A task carrying no
// push keeps the legacy project-scratch root, so an old engine/driver
// combination loses nothing it had.
func TestRunnerIOTaskNarrowsTheScratchRootsToThePush(t *testing.T) {
	t.Parallel()
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})

	// No push: the legacy fixed container path, no list.
	legacy := b.runnerIOTask(delegate.Task{NodeID: "n"}, nil)
	if legacy.ScratchDir != sandboxScratchContainerDir {
		t.Errorf("ScratchDir without a push = %q, want the legacy %q", legacy.ScratchDir, sandboxScratchContainerDir)
	}
	if len(legacy.ScratchReadRoots) != 0 {
		t.Errorf("ScratchReadRoots without a push = %v, want none", legacy.ScratchReadRoots)
	}

	// The narrowed push: subtree first, shared second; ScratchDir holds
	// the first so a runner that predates the list still narrows.
	narrowed := b.runnerIOTask(delegate.Task{
		NodeID:           "n",
		ScratchReadRoots: []string{sandboxScratchContainerDir + "/runs/run-1", sandboxScratchContainerDir},
	}, nil)
	if narrowed.ScratchDir != sandboxScratchContainerDir+"/runs/run-1" {
		t.Errorf("ScratchDir = %q, want the run subtree (the list's first)", narrowed.ScratchDir)
	}
	if len(narrowed.ScratchReadRoots) != 2 || narrowed.ScratchReadRoots[0] != sandboxScratchContainerDir+"/runs/run-1" ||
		narrowed.ScratchReadRoots[1] != sandboxScratchContainerDir {
		t.Errorf("ScratchReadRoots = %v, want [run subtree, shared]", narrowed.ScratchReadRoots)
	}

	// The subtree alone: a workflow that no longer references the shared
	// scratch crosses no shared root at all.
	alone := b.runnerIOTask(delegate.Task{
		NodeID:           "n",
		ScratchReadRoots: []string{sandboxScratchContainerDir + "/runs/run-2"},
	}, nil)
	if alone.ScratchDir != sandboxScratchContainerDir+"/runs/run-2" || len(alone.ScratchReadRoots) != 1 {
		t.Errorf("narrowed-alone envelope = %q %v, want the run subtree only", alone.ScratchDir, alone.ScratchReadRoots)
	}
}
