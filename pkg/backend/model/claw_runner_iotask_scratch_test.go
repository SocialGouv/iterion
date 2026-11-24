package model

import (
	"context"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// The sandbox crossing half of #2869: runnerIOTask narrows the
// in-container runner's scratch read roots to the ones the engine pushed
// (Task.ScratchReadRoots — the run family's subtree first, the shared
// project scratch behind it while still referenced — and
// Task.ScratchAnchor, the project scratch they lie beneath). A task
// carrying no push keeps the legacy project-scratch root, so an old
// engine/driver combination loses nothing it had.
func TestRunnerIOTaskNarrowsTheScratchRootsToThePush(t *testing.T) {
	t.Parallel()
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})
	base := sandboxScratchContainerDir
	subtree := base + "/runs/run-1"

	// No push: the legacy fixed container path, no list, no anchor.
	legacy := b.runnerIOTask(delegate.Task{NodeID: "n"}, nil)
	if legacy.ScratchDir != sandboxScratchContainerDir {
		t.Errorf("ScratchDir without a push = %q, want the legacy %q", legacy.ScratchDir, sandboxScratchContainerDir)
	}
	if len(legacy.ScratchReadRoots) != 0 || legacy.ScratchAnchor != "" {
		t.Errorf("ScratchReadRoots/ScratchAnchor without a push = %v %q, want none", legacy.ScratchReadRoots, legacy.ScratchAnchor)
	}

	// The push still referencing the shared scratch: subtree first, shared
	// second, the anchor crossing beside them. ScratchDir — the ONE root a
	// runner that predates the list registers — is the shared scratch: the
	// subtree lies beneath it, so the old runner reads exactly the union
	// the new one does, and the briefs every bot keeps under
	// ${PROJECT_SCRATCH_DIR} stay readable there.
	withShared := b.runnerIOTask(delegate.Task{
		NodeID:           "n",
		ScratchReadRoots: []string{subtree, base},
		ScratchAnchor:    base,
	}, nil)
	if withShared.ScratchDir != base {
		t.Errorf("ScratchDir = %q, want the shared scratch %q a predating runner still needs", withShared.ScratchDir, base)
	}
	if !slices.Equal(withShared.ScratchReadRoots, []string{subtree, base}) || withShared.ScratchAnchor != base {
		t.Errorf("envelope = %v anchored %q, want [run subtree, shared] anchored on the shared scratch", withShared.ScratchReadRoots, withShared.ScratchAnchor)
	}

	// The subtree alone: a workflow that no longer references the shared
	// scratch crosses no shared root at all, not even to an old runner.
	alone := b.runnerIOTask(delegate.Task{
		NodeID:           "n",
		ScratchReadRoots: []string{base + "/runs/run-2"},
		ScratchAnchor:    base,
	}, nil)
	if alone.ScratchDir != base+"/runs/run-2" || len(alone.ScratchReadRoots) != 1 {
		t.Errorf("narrowed-alone envelope = %q %v, want the run subtree only", alone.ScratchDir, alone.ScratchReadRoots)
	}
}

// The same crossing driven from the executor the engine pushes to: the
// SetScratchDirs push is stamped onto every task buildTask produces, and
// runnerIOTask narrows from that stamp. Without the stamp a sandboxed claw
// node falls back to the legacy constant — the whole project scratch —
// with nothing red, so this is the witness that the wiring holds, not
// just the crossing given a hand-built task.
func TestBuildTaskCarriesTheScratchPushToTheRunnerEnvelope(t *testing.T) {
	t.Parallel()
	base := sandboxScratchContainerDir
	subtree := base + "/runs/run-1"
	b := NewClawBackend(NewRegistry(), EventHooks{}, RetryPolicy{})

	for _, tc := range []struct {
		name      string
		shared    bool
		wantRoots []string
		wantDir   string
	}{
		{"narrowed", false, []string{subtree}, subtree},
		{"still referencing the shared scratch", true, []string{subtree, base}, base},
	} {
		e := &ClawExecutor{logger: iterlog.Nop(), sandbox: fakeSandboxRun{}}
		e.SetScratchDirs(base, subtree, tc.shared)
		node := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "reader"}}
		f := backendFields{id: "reader", model: "openai/gpt-5.6-sol"}
		task, err := e.buildTask(context.Background(), node, f, map[string]any{}, delegate.BackendClaw, nil)
		if err != nil {
			t.Fatalf("%s: buildTask: %v", tc.name, err)
		}
		io := b.runnerIOTask(task, nil)
		if !slices.Equal(io.ScratchReadRoots, tc.wantRoots) || io.ScratchAnchor != base || io.ScratchDir != tc.wantDir {
			t.Errorf("%s: runner envelope = roots %v anchor %q dir %q, want roots %v anchor %q dir %q",
				tc.name, io.ScratchReadRoots, io.ScratchAnchor, io.ScratchDir, tc.wantRoots, base, tc.wantDir)
		}
	}
}
