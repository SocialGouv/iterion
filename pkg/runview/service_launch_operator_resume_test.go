package runview

import (
	"context"
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

func (*operatorResumePublisher) SubmitLaunch(context.Context, string, LaunchSpec, *ir.Workflow, string) (int, error) {
	return 1, nil
}

func (*operatorResumePublisher) CancelRun(context.Context, string) error { return nil }

func (p *operatorResumePublisher) SubmitResume(context.Context, ResumeSpec, *ir.Workflow, string) error {
	p.resumeCalls++
	return nil
}

func TestValidateResumable_AcceptsPausedOperatorWithoutAnswers(t *testing.T) {
	r := &store.Run{ID: "run-operator-paused", Status: store.RunStatusPausedOperator}
	if err := validateResumable(r, nil); err != nil {
		t.Fatalf("validateResumable(paused_operator, nil) = %v, want nil", err)
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
