package cli_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestResume_theScratchsLossNeedsItsOwnConsent: `iterion resume --force`
// does not accept the loss of a run's scratch; --accept-scratch-loss
// (ResumeOptions.AcceptScratchLoss) does, and the run goes on.
func TestResume_theScratchsLossNeedsItsOwnConsent(t *testing.T) {
	hermeticSandbox(t)
	ctx := context.Background()
	dir := t.TempDir()
	botPath := writeFixture(t, dir, "operator.bot", `
workflow operator_resume:
  entry: done
`)
	storeDir := filepath.Join(dir, "store")
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRun(ctx, "scratch-consent", "operator_resume", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCheckpoint(ctx, r.ID, &store.Checkpoint{NodeID: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRunStatus(ctx, r.ID, store.RunStatusPausedOperator, "paused by operator"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AppendEvent(ctx, r.ID, store.Event{Type: store.EventSandboxScratchBanked, Data: map[string]any{
		"banked": false, "empty": false, "reason": "the scratch compresses past the 256 MiB cap",
	}}); err != nil {
		t.Fatal(err)
	}
	p, _ := newTestPrinter(cli.OutputHuman)
	err = cli.RunResumeWithFile(ctx, botPath, cli.ResumeOptions{RunID: r.ID, StoreDir: storeDir, Force: true}, p)
	var rt *runtime.RuntimeError
	if !errors.As(err, &rt) || rt.Code != runtime.ErrCodeScratchNotPortable {
		t.Fatalf("resume --force: %v, want the scratch's loss refused", err)
	}
	if err := cli.RunResumeWithFile(ctx, botPath, cli.ResumeOptions{RunID: r.ID, StoreDir: storeDir, AcceptScratchLoss: true}, p); err != nil {
		t.Fatalf("resume --accept-scratch-loss: %v", err)
	}
	if got, err := s.LoadRun(ctx, r.ID); err != nil || got.Status != store.RunStatusFinished {
		t.Fatalf("the accepting resume left the run %v (%v), want finished", got.Status, err)
	}
}
