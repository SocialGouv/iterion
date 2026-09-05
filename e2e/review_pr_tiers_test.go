package e2e

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Review-tier tests (native:685 / SocialGouv/iterion#685): prove the
// `review_tier` preset (glance/guard/audit) routes review-pr's REAL
// bots/review-pr/main.bot through the right reviewer nodes, using the
// deterministic scenario stub (no LLM) — the DSL-level guard for the
// tier_expand compute + topology-router wiring. Live cost validation is
// separate (needs provider credentials).

// reviewOutputStub returns a minimal, schema-valid review_output tagged
// with the given family.
func reviewOutputStub(family string) map[string]any {
	return map[string]any{
		"family": family, "findings": "[]", "scanned_areas": []string{},
		"questions": []string{}, "ticket_conformance": "", "summary": "clean",
		"_tokens": 10, "_cost_usd": 0.001,
	}
}

// wireReviewPRStubs wires the deterministic diff_precheck tool, all four
// reviewer node variants (only one or two of which ever fire per run), and
// converge — enough to reach `done` via the no-pr_url path (pr_gate ->
// done when not has_pr) without needing publish_review/publish_health.
func wireReviewPRStubs(exec *scenarioExecutor) {
	exec.on("diff_precheck", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"is_empty": false, "changed_files": 1, "reviewed_sha": "deadbeef",
			"_tokens": 1, "_cost_usd": 0,
		}, nil
	})
	for _, id := range []string{"reviewer_claude", "reviewer_claude_glance"} {
		exec.on(id, func(_ map[string]any) (map[string]any, error) {
			return reviewOutputStub("claude"), nil
		})
	}
	for _, id := range []string{"reviewer_gpt", "reviewer_gpt_glance"} {
		exec.on(id, func(_ map[string]any) (map[string]any, error) {
			return reviewOutputStub("gpt"), nil
		})
	}
	exec.on("converge", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{
			"report_path": "/tmp/review-pr-tier-test.md", "total_findings": 0,
			"issues_created": []string{}, "failed_issues": []string{},
			"findings": "[]", "questions": "", "ticket_conformance": "",
			"_tokens": 10, "_cost_usd": 0.001,
		}, nil
	})
}

func runReviewPRTier(t *testing.T, runID string, inputs map[string]any) *scenarioExecutor {
	t.Helper()
	wf := compileFixtureStubSafe(t, "review-pr/main.bot")
	exec := newScenarioExecutor()
	wireReviewPRStubs(exec)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), runID, inputs); err != nil {
		t.Fatalf("run error: %v", err)
	}
	r, _ := s.LoadRun(context.Background(), runID)
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished (tier routing never reached done?)", r.Status)
	}
	return exec
}

// Glance/claude: routes to the cheaper glance variant, never the
// full-strength reviewer, and never the second family.
func TestReviewPRTier_GlanceRoutesToCheapReviewer(t *testing.T) {
	exec := runReviewPRTier(t, "e2e-tier-glance-claude", map[string]any{
		"review_tier": "glance", "mono_family": "claude",
	})
	if !exec.wasCalled("reviewer_claude_glance") {
		t.Error("glance: reviewer_claude_glance never ran")
	}
	if exec.wasCalled("reviewer_claude") {
		t.Error("glance: the full-strength reviewer_claude ran instead of the glance variant")
	}
	if exec.wasCalled("reviewer_gpt") || exec.wasCalled("reviewer_gpt_glance") {
		t.Error("glance/mono: the second family must never fire")
	}
}

// Glance/gpt: symmetric — the gpt glance variant, never the full-strength
// reviewer_gpt.
func TestReviewPRTier_GlanceRoutesToCheapReviewerGPT(t *testing.T) {
	exec := runReviewPRTier(t, "e2e-tier-glance-gpt", map[string]any{
		"review_tier": "glance", "mono_family": "gpt",
	})
	if !exec.wasCalled("reviewer_gpt_glance") {
		t.Error("glance/gpt: reviewer_gpt_glance never ran")
	}
	if exec.wasCalled("reviewer_gpt") {
		t.Error("glance/gpt: the full-strength reviewer_gpt ran instead of the glance variant")
	}
}

// Guard (the default tier, no --var review_tier at all) reproduces the
// bot's pre-#685 behaviour: the full-strength reviewer, never the glance
// variant.
func TestReviewPRTier_GuardMatchesHistoricalDefaults(t *testing.T) {
	exec := runReviewPRTier(t, "e2e-tier-guard", map[string]any{
		"mono_family": "claude",
	})
	if !exec.wasCalled("reviewer_claude") {
		t.Error("guard: the full-strength reviewer_claude never ran")
	}
	if exec.wasCalled("reviewer_claude_glance") {
		t.Error("guard: the glance variant must not run under the default tier")
	}
}

// Audit forces the dual fan-out regardless of review_mode's own value
// (which stays at its "mono" default here) — audit's defining spend —
// and never routes through either glance variant. `fan` (fan_out_all) is a
// specially-dispatched router that never reaches the NodeExecutor, so its
// firing is asserted indirectly: both full-strength reviewers ran, which
// is only possible via the fan-out (mono routes to exactly one).
func TestReviewPRTier_AuditForcesDual(t *testing.T) {
	exec := runReviewPRTier(t, "e2e-tier-audit", map[string]any{
		"review_tier": "audit", "mono_family": "claude",
	})
	if !exec.wasCalled("reviewer_claude") || !exec.wasCalled("reviewer_gpt") {
		t.Error("audit: both full-strength families must run (the dual fan-out)")
	}
	if exec.wasCalled("reviewer_claude_glance") || exec.wasCalled("reviewer_gpt_glance") {
		t.Error("audit: never routes through the glance variants")
	}
}

// The tier is a preset, not a cage: an explicit review_mode=dual still
// forces the fan-out even under review_tier=glance (an unusual but
// expressible combination — the explicit knob wins). Same indirect
// assertion as TestReviewPRTier_AuditForcesDual — see its comment.
func TestReviewPRTier_ExplicitReviewModeWinsOverGlance(t *testing.T) {
	exec := runReviewPRTier(t, "e2e-tier-explicit-override", map[string]any{
		"review_tier": "glance", "review_mode": "dual", "mono_family": "claude",
	})
	if !exec.wasCalled("reviewer_claude") || !exec.wasCalled("reviewer_gpt") {
		t.Error("an explicit --var review_mode=dual must still force the fan-out under glance")
	}
}
