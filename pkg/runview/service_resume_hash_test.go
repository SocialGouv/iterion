package runview

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

const resumeHashBotOriginal = `
schema gate_out:
  approved: bool

prompt gate_prompt:
  Approve the original workflow?

human gate:
  instructions: gate_prompt
  output: gate_out
  interaction: human

workflow resume_hash:
  entry: gate
  gate -> done when approved
  gate -> fail when not approved
`

const resumeHashBotModified = `
schema gate_out:
  approved: bool

prompt gate_prompt:
  Approve the modified workflow?

human gate:
  instructions: gate_prompt
  output: gate_out
  interaction: human

workflow resume_hash:
  entry: gate
  gate -> done when approved
  gate -> fail when not approved
`

type resumeHashPublisher struct {
	resumeCalls int
}

func (*resumeHashPublisher) SubmitLaunch(context.Context, string, LaunchSpec, *ir.Workflow, string) (int, error) {
	return 1, nil
}

func (*resumeHashPublisher) CancelRun(context.Context, string) error {
	return nil
}

func (*resumeHashPublisher) CancelRunWithReason(context.Context, string, string) error {
	return nil
}

func (p *resumeHashPublisher) SubmitResume(context.Context, ResumeSpec, *ir.Workflow, string) error {
	p.resumeCalls++
	return nil
}

func seedChangedWorkflowRun(t *testing.T, svc *Service, runID, botPath string) {
	t.Helper()
	if _, err := svc.store.CreateRun(context.Background(), runID, "resume_hash", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := svc.store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	_, originalHash, err := CompileWorkflowFromSource(botPath, resumeHashBotOriginal)
	if err != nil {
		t.Fatalf("compile original workflow: %v", err)
	}
	r.Status = store.RunStatusFailedResumable
	r.FilePath = botPath
	r.WorkflowHash = originalHash
	if err := svc.store.SaveRun(context.Background(), r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
}

func newChangedWorkflowService(
	t *testing.T,
	publisher LaunchPublisher,
) (*Service, string) {
	t.Helper()
	dir := t.TempDir()
	botPath := filepath.Join(dir, "resume_hash.bot")
	if err := os.WriteFile(botPath, []byte(resumeHashBotModified), 0o644); err != nil {
		t.Fatalf("write modified workflow: %v", err)
	}
	opts := []ServiceOption{WithLogger(iterlog.Nop())}
	if publisher != nil {
		opts = append(opts, WithLaunchPublisher(publisher))
	}
	svc, err := NewService(dir, opts...)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	seedChangedWorkflowRun(t, svc, "run-source-changed", botPath)
	return svc, botPath
}

func TestServiceResumeRejectsChangedWorkflowSynchronouslyAcrossModes(t *testing.T) {
	tests := []struct {
		name      string
		detached  bool
		publisher bool
	}{
		{name: "in process"},
		{name: "detached", detached: true},
		{name: "cloud publisher", publisher: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.detached {
				t.Setenv(envDetached, "1")
			} else {
				t.Setenv(envDetached, "0")
			}
			var publisher *resumeHashPublisher
			if tt.publisher {
				publisher = &resumeHashPublisher{}
			}
			svc, botPath := newChangedWorkflowService(t, publisher)

			result, err := svc.Resume(context.Background(), ResumeSpec{
				RunID:    "run-source-changed",
				FilePath: botPath,
			})
			if result != nil {
				t.Fatalf("result = %#v, want nil on synchronous hash refusal", result)
			}
			if !errors.Is(err, runtime.ErrWorkflowSourceChanged) {
				t.Fatalf("error = %v, want ErrWorkflowSourceChanged", err)
			}
			if publisher != nil && publisher.resumeCalls != 0 {
				t.Fatalf("publisher called %d times before hash refusal, want 0", publisher.resumeCalls)
			}
		})
	}
}

func TestServiceResumeForceAllowsChangedWorkflow(t *testing.T) {
	t.Run("in process", func(t *testing.T) {
		t.Setenv(envDetached, "0")
		svc, botPath := newChangedWorkflowService(t, nil)

		result, err := svc.Resume(context.Background(), ResumeSpec{
			RunID:    "run-source-changed",
			FilePath: botPath,
			Force:    true,
		})
		if err != nil {
			t.Fatalf("forced Resume: %v", err)
		}
		if result == nil {
			t.Fatal("forced Resume returned nil result")
		}
		select {
		case <-result.Done:
		case <-time.After(5 * time.Second):
			t.Fatal("forced in-process resume did not terminate")
		}
	})

	t.Run("cloud publisher", func(t *testing.T) {
		t.Setenv(envDetached, "0")
		publisher := &resumeHashPublisher{}
		svc, botPath := newChangedWorkflowService(t, publisher)

		result, err := svc.Resume(context.Background(), ResumeSpec{
			RunID:    "run-source-changed",
			FilePath: botPath,
			Force:    true,
		})
		if err != nil {
			t.Fatalf("forced Resume: %v", err)
		}
		if result == nil {
			t.Fatal("forced Resume returned nil result")
		}
		if publisher.resumeCalls != 1 {
			t.Fatalf("publisher called %d times, want 1", publisher.resumeCalls)
		}
	})
}

func TestResume_RejectsWorkflowHashMismatchSynchronouslyBeforeSpawn(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "pause_demo.bot")
	if err := os.WriteFile(botPath, []byte(pausingBot), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}

	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	launched, err := svc.Launch(context.Background(), LaunchSpec{FilePath: botPath})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	select {
	case <-launched.Done:
	case <-time.After(30 * time.Second):
		t.Fatal("run did not reach its human pause")
	}

	before, err := svc.store.LoadRun(context.Background(), launched.RunID)
	if err != nil {
		t.Fatalf("LoadRun before resume: %v", err)
	}
	if before.Status != store.RunStatusPausedWaitingHuman {
		t.Fatalf("status before resume = %q, want paused_waiting_human", before.Status)
	}
	if before.Checkpoint == nil || before.Checkpoint.InteractionID == "" {
		t.Fatal("paused run has no interaction checkpoint")
	}

	// A harmless source edit still changes the persisted workflow hash and
	// must require the operator's explicit force confirmation.
	changedSource := "## workflow edited after the gate opened\n" + pausingBot
	if err := os.WriteFile(botPath, []byte(changedSource), 0o644); err != nil {
		t.Fatalf("rewrite bot: %v", err)
	}

	answers := map[string]any{"approve": true}
	_, err = svc.Resume(context.Background(), ResumeSpec{
		RunID:    launched.RunID,
		FilePath: botPath,
		Answers:  answers,
	})
	if err == nil {
		t.Fatal("Resume with a stale workflow hash returned nil, want a synchronous error")
	}
	if !strings.Contains(err.Error(), "workflow source has changed") {
		t.Fatalf("Resume error = %v, want workflow hash mismatch", err)
	}
	if svc.manager.Active(launched.RunID) {
		t.Fatal("stale resume registered an active run before rejecting the hash")
	}

	afterReject, err := svc.store.LoadRun(context.Background(), launched.RunID)
	if err != nil {
		t.Fatalf("LoadRun after rejected resume: %v", err)
	}
	if afterReject.Status != store.RunStatusPausedWaitingHuman {
		t.Fatalf("status after rejected resume = %q, want paused_waiting_human", afterReject.Status)
	}
	interaction, err := svc.store.LoadInteraction(
		context.Background(),
		launched.RunID,
		before.Checkpoint.InteractionID,
	)
	if err != nil {
		t.Fatalf("LoadInteraction after rejected resume: %v", err)
	}
	if interaction.AnsweredAt != nil || len(interaction.Answers) != 0 {
		t.Fatalf("rejected resume recorded answers: %#v", interaction.Answers)
	}

	forced, err := svc.Resume(context.Background(), ResumeSpec{
		RunID:    launched.RunID,
		FilePath: botPath,
		Answers:  answers,
		Force:    true,
	})
	if err != nil {
		t.Fatalf("forced Resume: %v", err)
	}
	select {
	case <-forced.Done:
	case <-time.After(30 * time.Second):
		t.Fatal("forced resume did not finish")
	}

	finished, err := svc.store.LoadRun(context.Background(), launched.RunID)
	if err != nil {
		t.Fatalf("LoadRun after forced resume: %v", err)
	}
	if finished.Status != store.RunStatusFinished {
		t.Fatalf("status after forced resume = %q (error %q), want finished", finished.Status, finished.Error)
	}
}

// PreflightResume must reject a stale-source resume BEFORE the caller
// performs anything irreversible. The HTTP layer promotes an operator's
// staged uploads into run attachments ahead of Resume, and promotion
// CONSUMES the staging — so a resume rejected afterwards leaves the
// studio's "Resume with updated workflow (force)" retry re-sending
// upload ids that no longer exist, making the documented recovery
// impossible.
func TestPreflightResumeRejectsStaleSourceAndAcceptsForce(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "preflight.bot")
	const src = `prompt ask_ok:
  Is this ok?

human gate:
  instructions: ask_ok

workflow preflight:
  entry: gate
  gate -> done
`
	if err := os.WriteFile(botPath, []byte(src), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithWorkDir(dir))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	const runID = "run-preflight"
	ctx := context.Background()
	if _, err := svc.store.CreateRun(ctx, runID, "preflight", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := svc.store.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	r.WorkflowHash = "0000000000000000deadbeef" // a source that is not this one
	if err := svc.store.SaveRun(ctx, r); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	if err := svc.store.SaveCheckpoint(ctx, runID, &store.Checkpoint{NodeID: "gate"}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if err := svc.store.UpdateRunStatus(ctx, runID, store.RunStatusPausedWaitingHuman, "paused"); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}

	spec := ResumeSpec{RunID: runID, FilePath: botPath, Answers: map[string]any{"ok": true}}
	err = svc.PreflightResume(ctx, spec)
	if err == nil {
		t.Fatal("preflight accepted a resume whose workflow source changed")
	}
	if !runtime.IsWorkflowSourceChanged(err) {
		t.Errorf("error = %v, want the workflow-source-changed error the studio keys its force retry on", err)
	}

	// The force retry is the documented recovery; preflight must not block it.
	spec.Force = true
	if err := svc.PreflightResume(ctx, spec); err != nil {
		t.Errorf("preflight rejected the force retry: %v", err)
	}
}
