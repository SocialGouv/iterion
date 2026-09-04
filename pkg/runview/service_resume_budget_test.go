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
