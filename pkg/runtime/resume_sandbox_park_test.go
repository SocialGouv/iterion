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

// ctxHonouringStore refuses every write the park issues once the ctx it
// is handed is done — what the Mongo driver does, and what the fs store
// (which ignores ctx) does not. The cancel and drain arms park on the
// very ctx that was just cancelled, so a test on the fs store alone
// certifies a stub.
type ctxHonouringStore struct{ store.RunStore }

func (s ctxHonouringStore) SaveCheckpoint(ctx context.Context, id string, cp *store.Checkpoint) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("save checkpoint %s: %w", id, err)
	}
	return s.RunStore.SaveCheckpoint(ctx, id, cp)
}

func (s ctxHonouringStore) UpdateRunStatusIfCoded(ctx context.Context, id string, status store.RunStatus, runErr string, code store.FailureCode, from []store.RunStatus) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, fmt.Errorf("update status if %s: %w", id, err)
	}
	return s.RunStore.UpdateRunStatusIfCoded(ctx, id, status, runErr, code, from)
}

func (s ctxHonouringStore) FailRunResumable(ctx context.Context, id string, cp *store.Checkpoint, runErr string, code store.FailureCode) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("fail resumable %s: %w", id, err)
	}
	return s.RunStore.FailRunResumable(ctx, id, cp, runErr, code)
}

func (s ctxHonouringStore) UpdateRunStatusCoded(ctx context.Context, id string, status store.RunStatus, runErr string, code store.FailureCode) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("update status %s: %w", id, err)
	}
	return s.RunStore.UpdateRunStatusCoded(ctx, id, status, runErr, code)
}

// The operator-cancel arm on a ctx-honouring store: the ctx is the one
// that was just cancelled, so a park written on it lands nowhere — the
// run reads `running` with the park logged as "context canceled". The
// write must ride a detached ctx, and the error handed to the runner must
// carry ErrRunCancelled so the delivery is acked, not burnt.
func TestParkResumeSandboxFailure_OperatorCancelLandsOnACtxHonouringStore(t *testing.T) {
	fs := tmpStore(t)
	e := &Engine{store: ctxHonouringStore{fs}, logger: iterlog.Nop()}
	const runID = "run-resume-sandbox-cancel-mongo"
	cp := seedRunningResume(t, fs, runID)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := e.parkResumeSandboxFailure(ctx, runID, cp, "entry", errors.New("docker start: context canceled"))

	r, _ := fs.LoadRun(context.Background(), runID)
	if r.Status != store.RunStatusCancelled || r.FailureCode != store.FailureCancelled {
		t.Fatalf("status/code = %s/%q, want cancelled/CANCELLED — the park was written on the cancelled ctx and a ctx-honouring store refused it (the run reads running forever on Mongo)", r.Status, r.FailureCode)
	}
	if r.Checkpoint == nil || r.Checkpoint.NodeID != "campaign" {
		t.Fatalf("checkpoint = %+v, want kept", r.Checkpoint)
	}
	if !errors.Is(err, ErrRunCancelled) {
		t.Fatalf("returned error = %v, want ErrRunCancelled — a bare wrap classifies as a generic failure and the runner naks an operator cancel", err)
	}
}

// The drain arm on a ctx-honouring store: same detached write, and the
// error carries ErrRunInterrupted so the runner naks it exempt from the
// DLQ park.
func TestParkResumeSandboxFailure_DrainLandsOnACtxHonouringStore(t *testing.T) {
	fs := tmpStore(t)
	e := &Engine{store: ctxHonouringStore{fs}, logger: iterlog.Nop()}
	const runID = "run-resume-sandbox-drain-mongo"
	cp := seedRunningResume(t, fs, runID)

	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(ErrRunInterrupted)
	err := e.parkResumeSandboxFailure(ctx, runID, cp, "entry", errors.New("kubectl exec: signal: killed"))

	r, _ := fs.LoadRun(context.Background(), runID)
	if r.Status != store.RunStatusFailedResumable || r.FailureCode != store.FailureInterrupted {
		t.Fatalf("status/code = %s/%q, want failed_resumable/INTERRUPTED — the park was refused on the cancelled ctx", r.Status, r.FailureCode)
	}
	if !errors.Is(err, ErrRunInterrupted) {
		t.Fatalf("returned error = %v, want ErrRunInterrupted — without it the drain misses the DLQ exemption", err)
	}
}

func TestParkResumeSandboxFailure_PhaseTimeoutIsTypedAndKeepsTheCheckpoint(t *testing.T) {
	s := tmpStore(t)
	e := &Engine{store: s, logger: iterlog.Nop()}
	const runID = "run-resume-sandbox-timeout"
	cp := seedRunningResume(t, s, runID)

	sbErr := fmt.Errorf("kubernetes: workspace copy phase timed out: %w",
		errors.Join(sandbox.ErrPhaseTimeout, context.DeadlineExceeded, errors.New("in-pod tar extract stalled")))
	perr := e.parkResumeSandboxFailure(context.Background(), runID, cp, "entry", sbErr)
	if !errors.Is(perr, sandbox.ErrPhaseTimeout) || errors.Is(perr, ErrRunInterrupted) || errors.Is(perr, ErrRunCancelled) {
		t.Fatalf("returned error = %v, want the driver's phase-timeout sentinel kept and no interruption/cancel dressing (the runner classifies it as a delayed nak)", perr)
	}

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
