package runview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

// E2: ResumeSpec.Budget is silently dropped on the in-process resume
// path (the runview.Service is built without a publisher and outside
// detached mode). Launch applies the override at service_launch.go:250;
// Resume did not, so the wf handed to BuildExecutor still carried the
// .bot's cap — a `--max-cost-usd 999` resume of a $60-capped run kept
// the $60 in the snapshot AND in the running engine's SharedBudget.
// Probe: seed a run with budget=$60 → in-process resume with
// spec.Budget=$120 → doc's r.Budget.MaxCostUSD stays $60 without the
// fix. The runResolveDoc snapshot is our oracle — same one the studio
// Overview reads.
const resumeBudgetInProcBot = `
workflow resume_budget_test:
  worktree: none
  repo_devbox: off
  budget:
    max_cost_usd: 60
    max_tokens: 5000
  entry: done
`

// E3: Resume must reject a malformed max_duration synchronously with
// an actionable error, else a typo ("4 hours") rides RunMessage.Budget
// to the runner, fails applyBudgetOverrides on every redelivery, and
// burns 8 deliveries into a DLQ park. Launch already does this
// (service_launch.go:128); Resume didn't.
func TestResume_RejectsMalformedBudgetDurationSynchronously(t *testing.T) {
	dir := t.TempDir()
	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, err = svc.Resume(context.Background(), ResumeSpec{
		RunID:    "run-any",
		FilePath: "wf.bot",
		Budget:   &ir.BudgetOverrides{MaxDuration: "4 hours"},
	})
	if err == nil {
		t.Fatal("Resume accepted a malformed max_duration; the runner would fail on every redelivery")
	}
	if !strings.Contains(err.Error(), "max_duration") {
		t.Fatalf("Resume error = %v, want it to name max_duration", err)
	}
}

// The launch ask must be persisted on the LOCAL run doc — only the cloud
// publisher wrote Run.BudgetOverrides, so on every local surface an
// ask-less resume replayed nothing and the operator's launch cap was
// silently replaced by the .bot's own. Probe: .bot cap $60, launch
// --max-cost-usd 120, $100 already spent, ask-less resume (the studio's
// answer-form path) → BUDGET_EXCEEDED "cost_usd (100/60)" while the doc
// still advertised 120.
func TestResume_AskLessLocalResumeKeepsTheLaunchCap(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "resume_budget_test.bot")
	if err := os.WriteFile(botPath, []byte(resumeBudgetInProcBot), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	t.Setenv(envDetached, "0")
	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx := context.Background()

	res, err := svc.Launch(ctx, LaunchSpec{FilePath: botPath, Budget: &ir.BudgetOverrides{MaxCostUSD: 120}})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	select {
	case <-res.Done:
	case <-time.After(30 * time.Second):
		t.Fatal("launch did not terminate")
	}
	r, err := svc.store.LoadRun(ctx, res.RunID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.BudgetOverrides == nil || r.BudgetOverrides.MaxCostUSD != 120 {
		t.Fatalf("run.BudgetOverrides after a local launch = %+v, want the $120 ask persisted — without it every local resume replays nothing", r.BudgetOverrides)
	}

	// $100 already spent, re-armed as resumable: the resume preflight
	// restores the spend and checks it against the cap the run resumes with.
	if err := svc.store.SaveCheckpoint(ctx, res.RunID, &store.Checkpoint{NodeID: "done", BudgetCostUSD: 100}); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	if err := svc.store.UpdateRunStatus(ctx, res.RunID, store.RunStatusFailedResumable, "budget exceeded"); err != nil {
		t.Fatalf("UpdateRunStatus: %v", err)
	}
	res2, err := svc.Resume(ctx, ResumeSpec{RunID: res.RunID, FilePath: botPath}) // ask-less
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	select {
	case <-res2.Done:
	case <-time.After(30 * time.Second):
		t.Fatal("resume did not terminate")
	}
	got, err := svc.store.LoadRun(ctx, res.RunID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got.Status != store.RunStatusFinished {
		t.Fatalf("status after the ask-less resume = %s (%s), want finished — the launch's $120 cap was replaced by the .bot's $60 on resume", got.Status, got.Error)
	}
}

// TestResume_AppliesBudgetOverridesInProcess covers the finding:
// spec.Budget must reach ApplyBudgetOverrides in the in-process path
// so the run doc's next snapshot reflects the raised cap.
func TestResume_AppliesBudgetOverridesInProcess(t *testing.T) {
	dir := t.TempDir()
	botPath := filepath.Join(dir, "resume_budget_test.bot")
	if err := os.WriteFile(botPath, []byte(resumeBudgetInProcBot), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}

	// No publisher, no detached. Force the in-process branch.
	t.Setenv(envDetached, "0")
	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	const runID = "run-inproc-resume-budget"
	_, workflowHash, err := CompileWorkflowWithHash(botPath)
	if err != nil {
		t.Fatalf("CompileWorkflowWithHash: %v", err)
	}
	seedPausedOperatorRun(t, svc, runID, workflowHash)

	res, err := svc.Resume(context.Background(), ResumeSpec{
		RunID:    runID,
		FilePath: botPath,
		Budget:   &ir.BudgetOverrides{MaxCostUSD: 120},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	select {
	case <-res.Done:
	case <-time.After(30 * time.Second):
		t.Fatal("in-process resume did not terminate")
	}
	r, err := svc.store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Budget == nil {
		t.Fatal("run.Budget = nil after resume; the runResolveDoc snapshot should exist")
	}
	if r.Budget.MaxCostUSD != 120 {
		t.Fatalf("run.Budget.MaxCostUSD = %v after resume with spec.Budget=$120, want 120 (E2: in-process resume dropped the override — the engine used the .bot's $60 in SharedBudget while the studio showed the same silent lie)", r.Budget.MaxCostUSD)
	}
	// The non-overridden field must survive as the .bot's value (per-field merge).
	if r.Budget.MaxTokens != 5000 {
		t.Fatalf("run.Budget.MaxTokens = %v, want 5000 (a partial override must not erase untouched caps)", r.Budget.MaxTokens)
	}
}
