package runview

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// budgetPausingBot pauses immediately on a human entry node (no LLM, no
// tools) so a Launch test can observe the persisted run doc — including
// the effective Budget snapshot stamped at runResolveDoc — without any
// backend credentials.
const budgetPausingBot = `
schema gate_out:
  approve: bool

prompt gate_prompt:
  Approve?

human gate:
  instructions: gate_prompt
  output: gate_out
  interaction: human

workflow budget_demo:
  ## This test runs from inside iterion, so the run's workspace root is the
  ## repo itself and both defaults bite: worktree defaults to auto (a full
  ## git checkout of iterion per run) and repo_devbox to on (realising
  ## iterion's own devbox.json, which a cold Nix cache turns into minutes).
  ## The test only reads the persisted budget snapshot, so it wants neither
  ## — together they made its wait for the human pause a coin flip.
  worktree: none
  repo_devbox: off
  budget:
    max_cost_usd: 60
    max_tokens: 5000
  entry: gate
  gate -> done when approve
  gate -> fail when not approve
`

func writeBudgetBot(t *testing.T, dir string) string {
	t.Helper()
	botPath := filepath.Join(dir, "budget_demo.bot")
	if err := os.WriteFile(botPath, []byte(budgetPausingBot), 0o644); err != nil {
		t.Fatalf("write bot: %v", err)
	}
	return botPath
}

// TestLaunch_AppliesBudgetOverrides verifies the LaunchSpec.Budget path
// end to end: overridden fields win, untouched fields inherit from the
// DSL budget: block, and the effective set is what the engine persists
// on the run doc (the same snapshot the studio Overview reads).
func TestLaunch_AppliesBudgetOverrides(t *testing.T) {
	dir := t.TempDir()
	botPath := writeBudgetBot(t, dir)

	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	res, err := svc.Launch(context.Background(), LaunchSpec{
		FilePath: botPath,
		Budget:   &ir.BudgetOverrides{MaxCostUSD: 120, MaxDuration: "4h"},
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	select {
	case <-res.Done:
	case <-time.After(30 * time.Second):
		t.Fatal("run goroutine did not exit (expected immediate human pause)")
	}

	r, err := svc.store.LoadRun(context.Background(), res.RunID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Budget == nil {
		t.Fatal("run.Budget = nil, want effective budget snapshot")
	}
	if r.Budget.MaxCostUSD != 120 {
		t.Errorf("MaxCostUSD = %v, want 120 (override wins)", r.Budget.MaxCostUSD)
	}
	if r.Budget.MaxDuration != "4h" {
		t.Errorf("MaxDuration = %q, want 4h (override wins)", r.Budget.MaxDuration)
	}
	if r.Budget.MaxTokens != 5000 {
		t.Errorf("MaxTokens = %d, want 5000 (zero override inherits DSL)", r.Budget.MaxTokens)
	}
}

// TestLaunch_RejectsInvalidBudgetDuration pins the fail-fast contract: a
// malformed MaxDuration aborts the launch synchronously, before any run
// doc is created.
func TestLaunch_RejectsInvalidBudgetDuration(t *testing.T) {
	dir := t.TempDir()
	botPath := writeBudgetBot(t, dir)

	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	_, err = svc.Launch(context.Background(), LaunchSpec{
		FilePath: botPath,
		Budget:   &ir.BudgetOverrides{MaxDuration: "4 hours"},
	})
	if err == nil {
		t.Fatal("Launch accepted a malformed max_duration, want error")
	}
	if !strings.Contains(err.Error(), "max_duration") {
		t.Errorf("error = %v, want it to name max_duration", err)
	}
}

// stubLaunchPublisher satisfies LaunchPublisher so tests can force the
// cloud-queue branch of Launch without a real NATS/Mongo backend. It
// records the last LaunchSpec so tests can assert what was forwarded.
type stubLaunchPublisher struct {
	lastSpec *LaunchSpec
}

func (p *stubLaunchPublisher) SubmitLaunch(_ context.Context, _ string, spec LaunchSpec, _ *ir.Workflow, _ string) (int, error) {
	p.lastSpec = &spec
	return 1, nil
}
func (p *stubLaunchPublisher) CancelRun(context.Context, string) error { return nil }
func (p *stubLaunchPublisher) CancelRunWithReason(context.Context, string, store.RunEndReason) error {
	return nil
}
func (p *stubLaunchPublisher) SubmitResume(context.Context, ResumeSpec, *ir.Workflow, string) error {
	return nil
}

// TestLaunch_CloudPathForwardsBudgetOverrides pins the queued-cloud
// contract: a non-zero Budget rides the LaunchSpec into the publisher
// (which puts it on queue.RunMessage.Budget for the runner) instead of
// being rejected — and a malformed one still fails synchronously.
func TestLaunch_CloudPathForwardsBudgetOverrides(t *testing.T) {
	dir := t.TempDir()
	botPath := writeBudgetBot(t, dir)

	pub := &stubLaunchPublisher{}
	svc, err := NewService(dir, WithLogger(iterlog.Nop()), WithLaunchPublisher(pub))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	if _, err := svc.Launch(context.Background(), LaunchSpec{
		FilePath: botPath,
		Budget:   &ir.BudgetOverrides{MaxCostUSD: 120},
	}); err != nil {
		t.Fatalf("cloud-path Launch with budget overrides: %v", err)
	}
	if pub.lastSpec == nil || pub.lastSpec.Budget == nil {
		t.Fatal("publisher did not receive the budget overrides")
	}
	if pub.lastSpec.Budget.MaxCostUSD != 120 {
		t.Errorf("forwarded MaxCostUSD = %v, want 120", pub.lastSpec.Budget.MaxCostUSD)
	}

	// A malformed override still fails the launch synchronously.
	if _, err := svc.Launch(context.Background(), LaunchSpec{
		FilePath: botPath,
		Budget:   &ir.BudgetOverrides{MaxDuration: "4 hours"},
	}); err == nil || !strings.Contains(err.Error(), "max_duration") {
		t.Errorf("malformed budget on cloud path: err = %v, want max_duration validation error", err)
	}

	// A launch WITHOUT overrides still goes through the publisher.
	if _, err := svc.Launch(context.Background(), LaunchSpec{FilePath: botPath}); err != nil {
		t.Fatalf("cloud-path Launch without budget: %v", err)
	}
}

// TestListCtx_BundleFilter verifies ListFilter.Bundle matches the
// resolved bundle name case-insensitively, including the legacy
// basename(BundlePath) fallback.
func TestListCtx_BundleFilter(t *testing.T) {
	dir := t.TempDir()
	svc, err := NewService(dir, WithLogger(iterlog.Nop()))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	seed := func(id, bundleName, bundlePath string) {
		t.Helper()
		if _, err := svc.store.CreateRun(context.Background(), id, "wf", nil); err != nil {
			t.Fatalf("CreateRun %s: %v", id, err)
		}
		r, err := svc.store.LoadRun(context.Background(), id)
		if err != nil {
			t.Fatalf("LoadRun %s: %v", id, err)
		}
		r.BundleName = bundleName
		r.BundlePath = bundlePath
		if err := svc.store.SaveRun(context.Background(), r); err != nil {
			t.Fatalf("SaveRun %s: %v", id, err)
		}
	}
	seed("run-docs", "docs-refresh", "")
	seed("run-feature", "", "/bots/feature-dev.botz") // legacy: name from path
	seed("run-plain", "", "")                         // plain .bot run, no bundle

	cases := []struct {
		bot  string
		want []string
	}{
		{"docs-refresh", []string{"run-docs"}},
		{"DOCS-Refresh", []string{"run-docs"}}, // case-insensitive
		{"feature-dev", []string{"run-feature"}},
		{"nonexistent", nil},
	}
	for _, tc := range cases {
		out, err := svc.ListCtx(context.Background(), ListFilter{Bundle: tc.bot})
		if err != nil {
			t.Fatalf("ListCtx(bot=%s): %v", tc.bot, err)
		}
		var got []string
		for _, r := range out {
			got = append(got, r.ID)
		}
		if len(got) != len(tc.want) {
			t.Errorf("bot=%q: got %v, want %v", tc.bot, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("bot=%q: got %v, want %v", tc.bot, got, tc.want)
			}
		}
	}

	// No filter still returns everything.
	all, err := svc.ListCtx(context.Background(), ListFilter{})
	if err != nil {
		t.Fatalf("ListCtx(all): %v", err)
	}
	if len(all) != 3 {
		t.Errorf("unfiltered len = %d, want 3", len(all))
	}
}
