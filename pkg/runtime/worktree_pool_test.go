package runtime

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/worktreepool"
)

// poolWorkflow is the smallest `worktree: auto` run there is: it creates a
// worktree, does nothing, and exits cleanly.
func poolWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "wt-pool",
		Entry: "start",
		Nodes: map[string]ir.Node{
			"start": &ir.ToolNode{BaseNode: ir.BaseNode{ID: "start"}, Command: "true"},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":  &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges:    []*ir.Edge{{From: "start", To: "done"}},
		Worktree: "auto",
	}
}

// parkWorktree leaves behind exactly what a finished run leaves when
// nothing reclaims it: a real linked worktree parked at the repository's
// own HEAD, with a terminal run record beside it.
func parkWorktree(t *testing.T, s store.RunStore, repo, runID string) string {
	t.Helper()
	path := filepath.Join(s.Root(), "worktrees", runID)
	mustRun(t, repo, "git", "worktree", "add", "--detach", path, "HEAD")
	if _, err := s.CreateRun(context.Background(), runID, "wt-pool", nil); err != nil {
		t.Fatalf("CreateRun(%s): %v", runID, err)
	}
	if err := s.UpdateRunStatus(context.Background(), runID, store.RunStatusFinished, ""); err != nil {
		t.Fatalf("UpdateRunStatus(%s): %v", runID, err)
	}
	return path
}

// The wiring, end to end: a run that creates a worktree brings the pool
// back under its ceiling on the way in. Unit tests cover what the bound
// decides; this covers that a run actually asks it.
func TestEngineRun_WorktreePoolIsBoundedOnTheWayIn(t *testing.T) {
	t.Setenv(worktreepool.BudgetEnv, "2")

	s := tmpStore(t)
	repo, _ := initBareishRepo(t)

	parked := []string{"old-1", "old-2", "old-3", "old-4"}
	for _, id := range parked {
		parkWorktree(t, s, repo, id)
	}

	var logBuf bytes.Buffer
	eng := New(poolWorkflow(), s, newStubExecutor(),
		WithWorkDir(repo),
		WithLogger(log.New(log.LevelInfo, &logBuf)),
	)
	if err := eng.Run(context.Background(), "fresh", nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}

	// The pass runs BEFORE the new worktree is created, so it trims the
	// four leftovers to the ceiling of two; the run's own worktree is then
	// created and removed again by its clean exit.
	survivors := 0
	for _, id := range parked {
		if _, err := os.Stat(filepath.Join(s.Root(), "worktrees", id)); err == nil {
			survivors++
		}
	}
	if survivors != 2 {
		t.Fatalf("%d of the 4 parked worktrees survived, want the budget of 2\nlog:\n%s", survivors, logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "worktree pool: reclaimed 2") {
		t.Errorf("the reclamation was not reported:\n%s", logBuf.String())
	}
}

// The budget is the operator's, and `off` means the engine does not touch
// the pool at all — the escape hatch every bound in this repo carries.
func TestEngineRun_WorktreePoolBudgetOffReclaimsNothing(t *testing.T) {
	t.Setenv(worktreepool.BudgetEnv, "off")

	s := tmpStore(t)
	repo, _ := initBareishRepo(t)
	for _, id := range []string{"old-1", "old-2", "old-3", "old-4"} {
		parkWorktree(t, s, repo, id)
	}

	eng := New(poolWorkflow(), s, newStubExecutor(),
		WithWorkDir(repo), WithLogger(log.New(log.LevelWarn, os.Stderr)))
	if err := eng.Run(context.Background(), "fresh", nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}

	for _, id := range []string{"old-1", "old-2", "old-3", "old-4"} {
		if _, err := os.Stat(filepath.Join(s.Root(), "worktrees", id)); err != nil {
			t.Fatalf("%s was reclaimed with the budget off: %v", id, err)
		}
	}
}

// A pool it cannot bring down must say so, name what is holding it up, and
// hand over a command — the whole point of preferring a warning to a
// refusal. Resumable runs are the case that matters: `iterion resume`
// restarts them in that very checkout, so the bound spares them and the
// operator, not the engine, decides to give the resume up.
func TestEngineRun_WorktreePoolWarnsWithTheReasonAndTheCommand(t *testing.T) {
	t.Setenv(worktreepool.BudgetEnv, "1")

	s := tmpStore(t)
	repo, _ := initBareishRepo(t)
	for _, id := range []string{"res-1", "res-2", "res-3"} {
		parkWorktree(t, s, repo, id)
		if err := s.UpdateRunStatus(context.Background(), id, store.RunStatusFailedResumable, "boom"); err != nil {
			t.Fatalf("UpdateRunStatus(%s): %v", id, err)
		}
	}

	var logBuf bytes.Buffer
	eng := New(poolWorkflow(), s, newStubExecutor(),
		WithWorkDir(repo), WithLogger(log.New(log.LevelInfo, &logBuf)))
	if err := eng.Run(context.Background(), "fresh", nil); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}

	for _, id := range []string{"res-1", "res-2", "res-3"} {
		if _, err := os.Stat(filepath.Join(s.Root(), "worktrees", id)); err != nil {
			t.Fatalf("the bound destroyed the resume of %s: %v", id, err)
		}
	}
	out := logBuf.String()
	for _, want := range []string{
		"exceed the budget of 1",
		"iterion resume` would restart",
		"iterion clean --store-dir",
		"--include-resumable",
		worktreepool.BudgetEnv,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the warning does not carry %q:\n%s", want, out)
		}
	}
}

func TestFormatWorktreePoolWarningWithoutSummaryHasCleanPunctuation(t *testing.T) {
	report := worktreepool.BudgetReport{
		Budget: 8,
		Total:  15,
		Before: 12,
		After:  12,
		Spared: map[string]int{},
	}
	got := formatWorktreePoolWarning(report, "/store")
	if strings.Contains(got, "; .") {
		t.Fatalf("warning has an empty summary clause: %q", got)
	}
	if !strings.Contains(got, "exceed the budget of 8. Raise or lift") {
		t.Fatalf("warning lost its actionable tail: %q", got)
	}
}

func TestFormatWorktreePoolWarningSeparatesParkedAndLiveCounts(t *testing.T) {
	report := worktreepool.BudgetReport{
		Budget: 8,
		Total:  16,
		Held:   4,
		Before: 12,
		After:  12,
		Spared: map[string]int{worktreepool.SkipLevel: 12},
	}
	got := formatWorktreePoolWarning(report, "/store")
	for _, want := range []string{"12 parked worktrees", "4 live worktrees excluded", "budget of 8"} {
		if !strings.Contains(got, want) {
			t.Errorf("warning does not contain %q: %s", want, got)
		}
	}
}
