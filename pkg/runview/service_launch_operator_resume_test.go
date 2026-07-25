package runview

import (
	"context"
	"os"
	"path/filepath"
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
	if _, err := svc.store.CreateRun(context.Background(), runID, "operator_resume", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := svc.store.SaveCheckpoint(context.Background(), runID, &store.Checkpoint{NodeID: "done"}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if err := svc.store.UpdateRunStatus(context.Background(), runID, store.RunStatusPausedOperator, "paused by operator"); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}

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
