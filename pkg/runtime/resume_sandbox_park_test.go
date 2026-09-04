package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/sandbox"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Both resume arms park a sandbox-start failure through one helper. What
// the arms used to write was FailRunResumable(…, sbErr.Error(), ""): the
// checkpoint survived but the cause did not — a sandbox phase timeout on
// a RESUMED run carried no SANDBOX_SETUP_TIMEOUT, exactly the shape #669
// measured (a resumed review stalled in sandbox start). The typed code is
// what the runner's retry lane and the gate notice read.

func seedRunningResume(t *testing.T, s store.RunStore, runID string) *store.Checkpoint {
	t.Helper()
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, runID, "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	cp := &store.Checkpoint{
		NodeID:  "campaign",
		Outputs: map[string]map[string]any{"plan": {"steps": 3}},
	}
	if err := s.SaveCheckpoint(ctx, runID, cp); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if err := s.UpdateRunStatus(ctx, runID, store.RunStatusRunning, ""); err != nil {
		t.Fatalf("UpdateRunStatus(running): %v", err)
	}
	return cp
}

func TestParkResumeSandboxFailure_PhaseTimeoutIsTypedAndKeepsTheCheckpoint(t *testing.T) {
	s := tmpStore(t)
	e := &Engine{store: s, logger: iterlog.Nop()}
	const runID = "run-resume-sandbox-timeout"
	cp := seedRunningResume(t, s, runID)

	sbErr := fmt.Errorf("kubernetes: workspace copy phase timed out: %w",
		errors.Join(sandbox.ErrPhaseTimeout, context.DeadlineExceeded, errors.New("in-pod tar extract stalled")))
	e.parkResumeSandboxFailure(context.Background(), runID, cp, "entry", sbErr)

	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %s, want failed_resumable (the documented resume-arm contract)", r.Status)
	}
	if r.FailureCode != store.FailureSandboxSetupTimeout {
		t.Fatalf("FailureCode = %q, want SANDBOX_SETUP_TIMEOUT — on a resumed run the phase timeout lands untyped, so the retry lane and the gate notice cannot tell it from a generic failure (#669's own shape)", r.FailureCode)
	}
	if !strings.Contains(r.Error, "sandbox setup phase timed out") || !strings.Contains(r.Error, "in-pod tar extract stalled") {
		t.Fatalf("Error = %q, want the classification's message with the cause kept", r.Error)
	}
	if r.Checkpoint == nil || r.Checkpoint.NodeID != "campaign" || r.Checkpoint.Outputs["plan"] == nil {
		t.Fatalf("checkpoint after the park = %+v, want the rich checkpoint preserved (a stub would restart the next resume from the entry)", r.Checkpoint)
	}
}

func TestParkResumeSandboxFailure_DrainIsInterruptedAndResumable(t *testing.T) {
	s := tmpStore(t)
	e := &Engine{store: s, logger: iterlog.Nop()}
	const runID = "run-resume-sandbox-drain"
	cp := seedRunningResume(t, s, runID)

	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrRunInterrupted)
	e.parkResumeSandboxFailure(ctx, runID, cp, "entry", errors.New("kubectl exec: signal: killed"))

	r, _ := s.LoadRun(context.Background(), runID)
	if r.Status != store.RunStatusFailedResumable || r.FailureCode != store.FailureInterrupted {
		t.Fatalf("status/code = %s/%q, want failed_resumable/INTERRUPTED", r.Status, r.FailureCode)
	}
}

func TestParkResumeSandboxFailure_OperatorCancelIsCancelledWithTheCheckpoint(t *testing.T) {
	s := tmpStore(t)
	e := &Engine{store: s, logger: iterlog.Nop()}
	const runID = "run-resume-sandbox-cancel"
	cp := seedRunningResume(t, s, runID)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	e.parkResumeSandboxFailure(ctx, runID, cp, "entry", errors.New("docker start: context canceled"))

	r, _ := s.LoadRun(context.Background(), runID)
	if r.Status != store.RunStatusCancelled || r.FailureCode != store.FailureCancelled {
		t.Fatalf("status/code = %s/%q, want cancelled/CANCELLED (the launch path says who stopped it; the resume arm must too)", r.Status, r.FailureCode)
	}
	if r.Checkpoint == nil || r.Checkpoint.NodeID != "campaign" {
		t.Fatalf("checkpoint after the cancel = %+v, want kept — cancelled is a resumable status", r.Checkpoint)
	}
}

func TestParkResumeSandboxFailure_PlainErrorStaysResumableAndUntyped(t *testing.T) {
	s := tmpStore(t)
	e := &Engine{store: s, logger: iterlog.Nop()}
	const runID = "run-resume-sandbox-plain"
	cp := seedRunningResume(t, s, runID)

	e.parkResumeSandboxFailure(context.Background(), runID, cp, "entry", errors.New("docker: image pull: connection reset"))

	r, _ := s.LoadRun(context.Background(), runID)
	if r.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %s, want failed_resumable — a docker hiccup on resume must stay resumable, unlike the launch path", r.Status)
	}
	if r.FailureCode != "" {
		t.Fatalf("FailureCode = %q, want unknown (empty) for a cause the taxonomy does not name", r.FailureCode)
	}
	if !strings.Contains(r.Error, "sandbox start") || !strings.Contains(r.Error, "connection reset") {
		t.Fatalf("Error = %q, want the phase and the cause", r.Error)
	}
}

// A run that never earned a checkpoint parks on the fallback node.
func TestParkResumeSandboxFailure_NoCheckpointParksOnTheFallbackNode(t *testing.T) {
	s := tmpStore(t)
	e := &Engine{store: s, logger: iterlog.Nop()}
	const runID = "run-resume-sandbox-nocp"
	if _, err := s.CreateRun(context.Background(), runID, "wf", nil); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRunStatus(context.Background(), runID, store.RunStatusRunning, ""); err != nil {
		t.Fatal(err)
	}

	e.parkResumeSandboxFailure(context.Background(), runID, nil, "plan", errors.New("boom"))

	r, _ := s.LoadRun(context.Background(), runID)
	if r.Checkpoint == nil || r.Checkpoint.NodeID != "plan" {
		t.Fatalf("checkpoint = %+v, want a stub on the fallback node", r.Checkpoint)
	}
}
