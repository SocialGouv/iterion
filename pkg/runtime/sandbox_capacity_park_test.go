package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// capacityStartError is the shape the kubernetes driver returns when the
// pod never got placed, wrapped the way engine_run.go wraps it.
func capacityStartError() error {
	return fmt.Errorf("runtime: sandbox: %w",
		fmt.Errorf("kubernetes: wait for pod ready: %w",
			errors.Join(sandbox.ErrCapacity,
				errors.New("0/12 nodes are available: 11 Insufficient cpu"))))
}

// The launch arm: a run whose sandbox never got placed must land
// failed_resumable + SANDBOX_CAPACITY. Classified `failed`, nothing ever
// comes back for it — the production shape of #699, where a dead review
// became a synthetic red gate that launched a fixer with nothing to fix.
func TestMarkFailedBestEffort_CapacityParksResumableAndTyped(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-sandbox-capacity"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	cp := &store.Checkpoint{NodeID: "campaign", Outputs: map[string]map[string]any{"plan": {"steps": 3}}}
	if err := s.SaveCheckpoint(ctx, runID, cp); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}

	eng := New(devboxTestWorkflow(), s, newStubExecutor(), WithLogger(iterlog.Nop()))
	eng.markFailedBestEffort(ctx, runID, "sandbox start", capacityStartError())

	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %q, want failed_resumable — an hourly sentinel silently loses its tick otherwise", r.Status)
	}
	if r.FailureCode != store.FailureSandboxCapacity {
		t.Fatalf("FailureCode = %q, want SANDBOX_CAPACITY", r.FailureCode)
	}
	if !r.Status.CanAutoResume() {
		t.Fatal("the retry machinery only picks up statuses CanAutoResume admits")
	}
	if r.Checkpoint == nil || r.Checkpoint.NodeID != "campaign" {
		t.Fatalf("checkpoint = %+v, want the one the run had earned — a park must never wipe it", r.Checkpoint)
	}
}

// The half that makes the resumable classification worth anything: a run
// that never started a node resumes from the workflow ENTRY. Without it a
// capacity park would be resumable in name only.
func TestResume_CapacityParkWithNoCheckpointRestartsFromTheEntry(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-capacity-resume-entry"
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	// Exactly what the launch arm leaves behind: no node ran, so no
	// checkpoint was ever saved.
	if err := s.UpdateRunStatusCoded(ctx, runID, store.RunStatusFailedResumable,
		"sandbox start: no capacity", store.FailureSandboxCapacity); err != nil {
		t.Fatalf("park: %v", err)
	}

	exec := newStubExecutor()
	ran := map[string]int{}
	exec.on("start", func(map[string]any) (map[string]any, error) {
		ran["start"]++
		return map[string]any{}, nil
	})

	eng := New(devboxTestWorkflow(), s, exec,
		WithWorkDir(t.TempDir()),
		WithSandboxOverride("none"),
		WithLogger(iterlog.Nop()),
	)
	if err := eng.Resume(ctx, runID, nil); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if ran["start"] != 1 {
		t.Fatalf("entry node ran %d times, want 1 — a run with no checkpoint must restart from the workflow entry", ran["start"])
	}
	r, err := s.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status after resume = %q, want finished", r.Status)
	}
}
