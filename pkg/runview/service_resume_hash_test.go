package runview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

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
