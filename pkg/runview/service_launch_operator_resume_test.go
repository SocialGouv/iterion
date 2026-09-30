package runview

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

type operatorResumePublisher struct {
	resumeCalls int
}

func (*operatorResumePublisher) SubmitLaunch(context.Context, string, LaunchSpec, *ir.Workflow, *CompiledSource) (int, error) {
	return 1, nil
}

func (*operatorResumePublisher) CancelRun(context.Context, string) error { return nil }
func (*operatorResumePublisher) CancelRunWithReason(context.Context, string, store.RunEndReason) error {
	return nil
}

func (p *operatorResumePublisher) SubmitResume(context.Context, ResumeSpec, *ir.Workflow, *CompiledSource) error {
	p.resumeCalls++
	return nil
}

func TestValidateResumable_AcceptsPausedOperatorWithoutAnswers(t *testing.T) {
	r := &store.Run{ID: "run-operator-paused", Status: store.RunStatusPausedOperator}
	if err := validateResumable(r, nil, false); err != nil {
		t.Fatalf("validateResumable(paused_operator, nil, false) = %v, want nil", err)
	}
}

// #663: validateResumable(automatic=true) uses CanAutoResume() — cancelled
// is refused there (an operator's cancel is a decision automation must never
// override). An in-flight sweeper resume that raced a stop-on-close cancel
// must not flip the doc back into a resumable shape.
func TestValidateResumable_AutomaticRefusesCancelled(t *testing.T) {
	r := &store.Run{ID: "run-cancelled", Status: store.RunStatusCancelled}
	if err := validateResumable(r, nil, true); err == nil {
		t.Fatal("validateResumable(cancelled, automatic=true) = nil, want refusal — the sweeper must never override an operator's cancel")
	}
	// Same doc, operator-initiated → still accepted (paused/cancelled are
	// resumable when the operator explicitly asks).
	if err := validateResumable(r, nil, false); err != nil {
		t.Fatalf("validateResumable(cancelled, automatic=false) = %v, want nil — operator resume of a cancelled run is a supported path", err)
	}
}

func seedPausedOperatorRun(t *testing.T, svc *Service, runID, workflowHash string) {
	t.Helper()
	if _, err := svc.store.CreateRun(context.Background(), runID, "operator_resume", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := svc.store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.WorkflowHash = workflowHash
	if err := svc.store.SaveRun(context.Background(), r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	if err := svc.store.SaveCheckpoint(context.Background(), runID, &store.Checkpoint{NodeID: "done"}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if err := svc.store.UpdateRunStatus(context.Background(), runID, store.RunStatusPausedOperator, "paused by operator"); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
}

func TestResume_AcceptsPausedOperatorWithoutAnswers(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "operator_resume.bot")
	const source = `
workflow operator_resume:
  entry: done
`
	if err := os.WriteFile(botPath, []byte(source), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}

	publisher := &operatorResumePublisher{}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithLaunchPublisher(publisher))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	const runID = "run-operator-paused"
	_, workflowHash, err := CompileWorkflowWithHash(botPath)
	if err != nil {
		t.Fatalf("CompileWorkflowWithHash: %v", err)
	}
	seedPausedOperatorRun(t, svc, runID, workflowHash)

	res, err := svc.Resume(context.Background(), ResumeSpec{RunID: runID, FilePath: botPath})
	if err != nil {
		t.Fatalf("Resume paused_operator: %v", err)
	}
	if res.RunID != runID {
		t.Errorf("Resume RunID = %q, want %q", res.RunID, runID)
	}
	if publisher.resumeCalls != 1 {
		t.Errorf("SubmitResume calls = %d, want 1", publisher.resumeCalls)
	}
}

func TestResume_RejectsWorkflowHashMismatchBeforePublishing(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "operator_resume.bot")
	const source = `
workflow operator_resume:
  entry: done
`
	if err := os.WriteFile(botPath, []byte(source), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}

	publisher := &operatorResumePublisher{}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithLaunchPublisher(publisher))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	const runID = "run-operator-stale"
	seedPausedOperatorRun(t, svc, runID, strings.Repeat("0", 64))

	_, err = svc.Resume(context.Background(), ResumeSpec{RunID: runID, FilePath: botPath})
	if err == nil {
		t.Fatal("Resume with a stale workflow hash returned nil, want a synchronous error")
	}
	if !strings.Contains(err.Error(), "workflow source has changed") {
		t.Fatalf("Resume error = %v, want workflow hash mismatch", err)
	}
	if publisher.resumeCalls != 0 {
		t.Fatalf("SubmitResume calls after stale preflight = %d, want 0", publisher.resumeCalls)
	}
	r, loadErr := svc.store.LoadRun(context.Background(), runID)
	if loadErr != nil {
		t.Fatalf("LoadRun after rejected resume: %v", loadErr)
	}
	if r.Status != store.RunStatusPausedOperator {
		t.Fatalf("status after rejected resume = %q, want paused_operator", r.Status)
	}

	if _, err := svc.Resume(context.Background(), ResumeSpec{
		RunID:    runID,
		FilePath: botPath,
		Force:    true,
	}); err != nil {
		t.Fatalf("forced Resume with a stale workflow hash: %v", err)
	}
	if publisher.resumeCalls != 1 {
		t.Fatalf("SubmitResume calls after forced resume = %d, want 1", publisher.resumeCalls)
	}
}

// TestResume_RefusesAScratchThatDidNotTravelBeforePublishing: a run whose
// last teardown left a scratch it could not bank is refused by the resume
// surface itself — synchronously, before the publisher flips it to queued,
// where the engine's own refusal would leave it until the orphan sweeper.
// --force publishes.
func TestResume_RefusesAScratchThatDidNotTravelBeforePublishing(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "operator_resume.bot")
	const source = `
workflow operator_resume:
  entry: done
`
	if err := os.WriteFile(botPath, []byte(source), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	publisher := &operatorResumePublisher{}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithLaunchPublisher(publisher))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const runID = "run-operator-scratch"
	_, workflowHash, err := CompileWorkflowWithHash(botPath)
	if err != nil {
		t.Fatalf("CompileWorkflowWithHash: %v", err)
	}
	seedPausedOperatorRun(t, svc, runID, workflowHash)
	if _, err := svc.store.AppendEvent(context.Background(), runID, store.Event{Type: store.EventSandboxScratchBanked, Data: map[string]any{
		"banked": false, "empty": false, "reason": "the scratch compresses past the 256 MiB cap",
	}}); err != nil {
		t.Fatal(err)
	}

	_, err = svc.Resume(context.Background(), ResumeSpec{RunID: runID, FilePath: botPath})
	var rt *runtime.RuntimeError
	if !errors.As(err, &rt) || rt.Code != runtime.ErrCodeScratchNotPortable {
		t.Fatalf("Resume of a run whose scratch did not travel = %v, want SCRATCH_NOT_PORTABLE", err)
	}
	if publisher.resumeCalls != 0 {
		t.Fatalf("SubmitResume calls after the refusal = %d, want 0", publisher.resumeCalls)
	}
	if r, _ := svc.store.LoadRun(context.Background(), runID); r.Status != store.RunStatusPausedOperator {
		t.Fatalf("status after the refusal = %q, want paused_operator", r.Status)
	}
	if _, err := svc.Resume(context.Background(), ResumeSpec{RunID: runID, FilePath: botPath, Force: true}); err != nil {
		t.Fatalf("forced Resume: %v", err)
	}
	if publisher.resumeCalls != 1 {
		t.Fatalf("SubmitResume calls after the forced resume = %d, want 1", publisher.resumeCalls)
	}
}

// scratchRefused reports err as the engine's refusal of a resume that would
// lose or revert its scratch.
func scratchRefused(err error) bool {
	var rt *runtime.RuntimeError
	return errors.As(err, &rt) && rt.Code == runtime.ErrCodeScratchNotPortable
}

// TestResume_refusesALoneChildBeforePublishing: a child that executed in a
// parent sandbox whose scratch lived in the container is refused by the
// surface, as the engine refuses it, before the publisher moves the run.
func TestResume_refusesALoneChildBeforePublishing(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "operator_resume.bot")
	if err := os.WriteFile(botPath, []byte("\nworkflow operator_resume:\n  entry: done\n"), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	publisher := &operatorResumePublisher{}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithLaunchPublisher(publisher))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const runID = "run-child-lone"
	_, workflowHash, err := CompileWorkflowWithHash(botPath)
	if err != nil {
		t.Fatalf("CompileWorkflowWithHash: %v", err)
	}
	seedPausedOperatorRun(t, svc, runID, workflowHash)
	ctx := context.Background()
	r, err := svc.store.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	r.ParentRunID = "run-parent"
	if err := svc.store.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.store.AppendEvent(ctx, runID, store.Event{Type: store.EventSandboxShared, Data: map[string]any{
		"adopted": true, "driver": "docker", "parent_run": "run-parent", "copy_based": false, "scratch_container_local": true,
	}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.PreflightResume(ctx, ResumeSpec{RunID: runID, FilePath: botPath}); !scratchRefused(err) {
		t.Fatalf("PreflightResume of a lone child: %v, want SCRATCH_NOT_PORTABLE", err)
	}
	if _, err := svc.Resume(ctx, ResumeSpec{RunID: runID, FilePath: botPath}); !scratchRefused(err) || publisher.resumeCalls != 0 {
		t.Fatalf("Resume of a lone child: %v, published %d, want SCRATCH_NOT_PORTABLE and nothing published", err, publisher.resumeCalls)
	}
}

// TestResume_theScratchRefusalComesBeforeTheSourceCheck: a run whose source
// changed and whose scratch did not travel is refused over the scratch first
// — the force the studio offers for an edited source would otherwise waive a
// loss it never showed.
func TestResume_theScratchRefusalComesBeforeTheSourceCheck(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "operator_resume.bot")
	if err := os.WriteFile(botPath, []byte("\nworkflow operator_resume:\n  entry: done\n"), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	publisher := &operatorResumePublisher{}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithLaunchPublisher(publisher))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	const runID = "run-source-and-scratch"
	_, hash, err := CompileWorkflowWithHash(botPath)
	if err != nil {
		t.Fatalf("CompileWorkflowWithHash: %v", err)
	}
	seedPausedOperatorRun(t, svc, runID, hash)
	ctx := context.Background()
	if _, err := svc.store.AppendEvent(ctx, runID, store.Event{Type: store.EventSandboxScratchBanked, Data: map[string]any{
		"banked": false, "empty": false, "reason": "the scratch compresses past the 256 MiB cap",
	}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(botPath, []byte("\n## edited\nworkflow operator_resume:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := svc.PreflightResume(ctx, ResumeSpec{RunID: runID, FilePath: botPath}); !scratchRefused(err) || runtime.IsWorkflowSourceChanged(err) || !namesSourceChange(err) {
		t.Fatalf("PreflightResume over an edited source and a lost scratch: %v, want SCRATCH_NOT_PORTABLE first, naming the source change", err)
	}
	if _, err := svc.Resume(ctx, ResumeSpec{RunID: runID, FilePath: botPath}); !scratchRefused(err) || runtime.IsWorkflowSourceChanged(err) || !namesSourceChange(err) || publisher.resumeCalls != 0 {
		t.Fatalf("Resume over an edited source and a lost scratch: %v, published %d, want SCRATCH_NOT_PORTABLE first, naming the source change, and nothing published", err, publisher.resumeCalls)
	}
}
