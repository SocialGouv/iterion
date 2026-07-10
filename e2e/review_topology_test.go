package e2e

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Review-topology tests: prove the condition-router + topology-aware stop
// used by the bi-model review-loop bots routes correctly in both dual and
// mono, using the deterministic scenario stub (no LLM). This is the
// runnable-now guard for the round_robin→condition migration; the live
// asymptote/dogfood validation is separate (needs provider credentials).

// approvingReviewers wires both family reviewers to always approve with a
// high-confidence verdict tagged with their family.
func approvingReviewers(exec *scenarioExecutor) {
	exec.on("seed", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"approved": false, "family": "", "confidence": "high",
			"_tokens": 10, "_cost_usd": 0.001,
		}, nil
	})
	exec.on("reviewer_claude", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"approved": true, "family": "claude", "confidence": "high",
			"_tokens": 10, "_cost_usd": 0.001,
		}, nil
	})
	exec.on("reviewer_gpt", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"approved": true, "family": "gpt", "confidence": "high",
			"_tokens": 10, "_cost_usd": 0.001,
		}, nil
	})
}

func runTopology(t *testing.T, runID string, inputs map[string]any) *scenarioExecutor {
	t.Helper()
	wf := compileFixture(t, "review_topology_mini.bot")
	exec := newScenarioExecutor()
	approvingReviewers(exec)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), runID, inputs); err != nil {
		t.Fatalf("run error: %v", err)
	}
	r, _ := s.LoadRun(context.Background(), runID)
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished (topology never converged?)", r.Status)
	}
	return exec
}

// Dual: both families must run (alternation), converging on the first
// cross-family double-approval (pass 0 claude, pass 1 gpt → stop).
func TestReviewTopology_DualAlternates(t *testing.T) {
	exec := runTopology(t, "e2e-topo-dual", map[string]any{"review_mode": "dual"})
	if !exec.wasCalled("reviewer_claude") {
		t.Error("dual: reviewer_claude never ran")
	}
	if !exec.wasCalled("reviewer_gpt") {
		t.Error("dual: reviewer_gpt never ran (alternation broken)")
	}
	// First pass must be claude (parity 0 → the unconditional default),
	// mirroring the old round_robin first target.
	if first := firstReviewer(exec); first != "reviewer_claude" {
		t.Errorf("dual: first reviewer = %q, want reviewer_claude", first)
	}
}

// firstReviewer returns the node ID of the first reviewer_* the run
// executed (the calls log also contains seed/alt/streak_check).
func firstReviewer(exec *scenarioExecutor) string {
	exec.mu.Lock()
	defer exec.mu.Unlock()
	for _, id := range exec.calls {
		if id == "reviewer_claude" || id == "reviewer_gpt" {
			return id
		}
	}
	return ""
}

// Auto with no explicit mode still behaves dual (parity) — the
// non-regression path when the resolver isn't wired.
func TestReviewTopology_AutoDefaultsDual(t *testing.T) {
	exec := runTopology(t, "e2e-topo-auto", nil)
	if !exec.wasCalled("reviewer_gpt") {
		t.Error("auto: expected dual behaviour (gpt should run), gpt never ran")
	}
}

// Mono/claude: ONLY claude runs; gpt is never spawned (the frugality
// guarantee). Converges on two consecutive self-approvals.
func TestReviewTopology_MonoClaudeSingleFamily(t *testing.T) {
	exec := runTopology(t, "e2e-topo-mono-claude", map[string]any{
		"review_mode": "mono", "mono_family": "claude",
	})
	if exec.wasCalled("reviewer_gpt") {
		t.Errorf("mono/claude: reviewer_gpt ran %d× — the second family must never fire", exec.callCount("reviewer_gpt"))
	}
	if exec.callCount("reviewer_claude") < 2 {
		t.Errorf("mono/claude: reviewer_claude ran %d×, want ≥2 (two clean passes to stop)", exec.callCount("reviewer_claude"))
	}
}

// Mono/gpt: symmetric — only gpt runs.
func TestReviewTopology_MonoGptSingleFamily(t *testing.T) {
	exec := runTopology(t, "e2e-topo-mono-gpt", map[string]any{
		"review_mode": "mono", "mono_family": "gpt",
	})
	if exec.wasCalled("reviewer_claude") {
		t.Errorf("mono/gpt: reviewer_claude ran %d× — the second family must never fire", exec.callCount("reviewer_claude"))
	}
	if exec.callCount("reviewer_gpt") < 2 {
		t.Errorf("mono/gpt: reviewer_gpt ran %d×, want ≥2", exec.callCount("reviewer_gpt"))
	}
}
