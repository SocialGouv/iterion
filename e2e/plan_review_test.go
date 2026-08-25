package e2e

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// Plan-phase tests: prove the launch-resolved `plan_review` gate + the
// author → cross-model peer → author-revise chain used by the campaign
// bots routes correctly, with the deterministic scenario stub (no LLM).
// The `action: skip` fallback itself is an executor-level feature
// (pkg/backend/model chain_skip_test.go); here the skipped case is
// simulated by the peer emitting the `_skipped` stamp the plan_gate
// reads — the same contract the executor synthesizes.

// planReviewStubs wires the phase's stub handlers and returns a pointer
// to the input map the campaign was handed (the hand-off under test).
func planReviewStubs(exec *scenarioExecutor, reviewSkipped bool, campaignInput *map[string]any) {
	exec.on("plan", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"plan": "slice 1, slice 2", "_tokens": 10, "_cost_usd": 0.001}, nil
	})
	exec.on("plan_review", func(_ map[string]any) (map[string]any, error) {
		out := map[string]any{"concerns": "slice 2 duplicates a seam", "blocking": false,
			"_tokens": 10, "_cost_usd": 0.001}
		if reviewSkipped {
			out = map[string]any{"concerns": "", "blocking": false,
				"_skipped": true, "_fallback_used": true, "_served_by": "give_up",
				"_tokens": 0, "_cost_usd": 0.0}
		}
		return out, nil
	})
	exec.on("plan_revise", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"plan": "slice 1 only", "review_responses": "accepted: reuse the seam",
			"_tokens": 10, "_cost_usd": 0.001}, nil
	})
	exec.on("campaign", func(in map[string]any) (map[string]any, error) {
		if campaignInput != nil {
			*campaignInput = in
		}
		return map[string]any{"done": true, "_tokens": 10, "_cost_usd": 0.001}, nil
	})
}

func runPlanReview(t *testing.T, runID string, inputs map[string]any, reviewSkipped bool) (*scenarioExecutor, map[string]any) {
	t.Helper()
	wf := compileFixture(t, "plan_review_mini.bot")
	exec := newScenarioExecutor()
	var campaignIn map[string]any
	planReviewStubs(exec, reviewSkipped, &campaignIn)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), runID, inputs); err != nil {
		t.Fatalf("run error: %v", err)
	}
	r, _ := s.LoadRun(context.Background(), runID)
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished", r.Status)
	}
	return exec, campaignIn
}

// plan_review off (creds-unresolved / operator off) → the whole phase is
// bypassed: entry routes straight to the campaign, zero plan-phase cost.
func TestPlanReview_OffBypassesThePhase(t *testing.T) {
	exec, _ := runPlanReview(t, "e2e-plan-off", map[string]any{"plan_review": "off"}, false)
	for _, id := range []string{"plan", "plan_review", "plan_revise"} {
		if exec.wasCalled(id) {
			t.Errorf("off: %s ran — the phase must be bypassed whole", id)
		}
	}
	if !exec.wasCalled("campaign") {
		t.Error("off: campaign never ran")
	}
}

// The bot's own default ("auto", launch resolver not wired in this
// harness) must behave exactly like off — the conservative default.
func TestPlanReview_AutoUnresolvedBehavesOff(t *testing.T) {
	exec, _ := runPlanReview(t, "e2e-plan-auto", nil, false)
	if exec.wasCalled("plan") {
		t.Error("auto/unresolved: plan ran — an unresolved auto must not open the phase")
	}
}

// plan_review on, peer serves → author → peer → author-revise → campaign,
// with the revised plan + critique handed to the campaign.
func TestPlanReview_OnRunsAuthorPeerRevise(t *testing.T) {
	exec, in := runPlanReview(t, "e2e-plan-on", map[string]any{"plan_review": "on"}, false)
	for _, id := range []string{"plan", "plan_review", "plan_revise", "campaign"} {
		if !exec.wasCalled(id) {
			t.Errorf("on: %s never ran", id)
		}
	}
	if in["plan"] != "slice 1 only" {
		t.Errorf("campaign received plan %q, want the REVISED plan", in["plan"])
	}
	if in["plan_critique"] != "slice 2 duplicates a seam" {
		t.Errorf("campaign received critique %q", in["plan_critique"])
	}
	if in["plan_responses"] != "accepted: reuse the seam" {
		t.Errorf("campaign received responses %q", in["plan_responses"])
	}
}

// Peer skipped (the executor's `action: skip` route fired — simulated by
// the _skipped stamp) → the revise turn is bypassed and the UNREVISED
// plan reaches the campaign, with an empty critique.
func TestPlanReview_SkippedRoutesAroundRevise(t *testing.T) {
	exec, in := runPlanReview(t, "e2e-plan-skip", map[string]any{
		"plan_review": "on", "plan_review_policy": "skip",
	}, true)
	if exec.wasCalled("plan_revise") {
		t.Error("skipped: plan_revise ran — a skipped review has nothing to challenge")
	}
	if in["plan"] != "slice 1, slice 2" {
		t.Errorf("campaign received plan %q, want the AUTHOR's plan", in["plan"])
	}
	if c, ok := in["plan_critique"]; ok && c != "" {
		t.Errorf("campaign received critique %q, want empty on a skipped review", c)
	}
}
