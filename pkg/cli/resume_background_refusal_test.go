package cli_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/cli"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestResume_inTheBackgroundARefusalBeforeTheClaimIsLogged: a managed
// runner's stdio goes nowhere, and a resume its engine refuses before the
// claim — a bank gone stale behind an execution that wrote no record —
// leaves the run where it was. The refusal is said in the run's log, where
// the studio reads the run.
func TestResume_inTheBackgroundARefusalBeforeTheClaimIsLogged(t *testing.T) {
	hermeticSandbox(t)
	t.Chdir(gittest.SourceRepo(t))
	ctx := context.Background()
	dir := t.TempDir()
	bot := filepath.Join(dir, "stale.bot")
	if err := os.WriteFile(bot, []byte("\nworkflow stale:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, hash, err := runview.CompileWorkflowWithHash(bot)
	if err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Join(dir, "store")
	s, err := store.New(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CreateRun(ctx, "run-background-stale", "stale", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCheckpoint(ctx, r.ID, &store.Checkpoint{NodeID: "done"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRunStatus(ctx, r.ID, store.RunStatusPausedOperator, "paused by operator"); err != nil {
		t.Fatal(err)
	}
	if r, err = s.LoadRun(ctx, r.ID); err != nil {
		t.Fatal(err)
	}
	r.WorkflowHash = hash
	r.FilePath = bot
	if err := s.SaveRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []store.Event{
		{Type: store.EventSandboxScratchBanked, Data: map[string]any{"banked": true, "bytes": 42}},
		{Type: store.EventRunResumed},
		{Type: store.EventSandboxScratchRestored, Data: map[string]any{"restored": true, "bytes": 42}},
		{Type: store.EventNodeFinished, NodeID: "work", Data: map[string]any{"_in_sandbox": true, "_on_cycle": false}},
	} {
		if _, err := s.AppendEvent(ctx, r.ID, ev); err != nil {
			t.Fatal(err)
		}
	}

	p, _ := newTestPrinter(cli.OutputHuman)
	err = cli.RunResumeWithFile(ctx, bot, cli.ResumeOptions{RunID: r.ID, StoreDir: storeDir, Background: true}, p)
	var rt *runtime.RuntimeError
	if !errors.As(err, &rt) || rt.Code != runtime.ErrCodeScratchNotPortable {
		t.Fatalf("a background resume of a stale bank: %v, want SCRATCH_NOT_PORTABLE", err)
	}
	log, err := os.ReadFile(filepath.Join(storeDir, "runs", r.ID, "run.log"))
	if err != nil {
		t.Fatalf("the run's log: %v", err)
	}
	if !strings.Contains(string(log), "refused before it claimed the run") || !strings.Contains(string(log), string(runtime.ErrCodeScratchNotPortable)) {
		t.Fatalf("the run's log does not say the resume was refused:\n%s", log)
	}
	if r, err := s.LoadRun(ctx, r.ID); err != nil || r.Status != store.RunStatusPausedOperator {
		t.Fatalf("the refused resume moved the run: %v (%v)", r.Status, err)
	}
}
