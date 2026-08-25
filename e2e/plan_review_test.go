package e2e

import (
	"context"
	"strings"
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
//
// The fixture's with{} bodies MIRROR the shipped bots', and the
// assertions REQUIRE the hand-off fields to be present-and-empty — an
// unmapped field is not "" but the raw "{{input.x}}" placeholder
// leaking into the campaign prompt, which is exactly the defect these
// tests exist to refuse.

// planReviewStubs wires the phase's stub handlers. campaignInputs
// collects the input map of EVERY campaign pass (the hand-off under
// test); the campaign converges on its second pass so the continuation
// loop-back is exercised once.
func planReviewStubs(exec *scenarioExecutor, reviewSkipped bool, campaignInputs *[]map[string]any) {
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
		if campaignInputs != nil {
			*campaignInputs = append(*campaignInputs, in)
		}
		// Converge on the second pass so the loop back-edge runs once.
		done := campaignInputs != nil && len(*campaignInputs) >= 2
		return map[string]any{"done": done, "_tokens": 10, "_cost_usd": 0.001}, nil
	})
}

func runPlanReview(t *testing.T, runID string, inputs map[string]any, reviewSkipped bool) (*scenarioExecutor, []map[string]any) {
	t.Helper()
	wf := compileFixture(t, "plan_review_mini.bot")
	exec := newScenarioExecutor()
	var campaignIns []map[string]any
	planReviewStubs(exec, reviewSkipped, &campaignIns)

	s := tmpStore(t)
	eng := runtime.New(wf, s, exec)
	if err := eng.Run(context.Background(), runID, inputs); err != nil {
		t.Fatalf("run error: %v", err)
	}
	r, _ := s.LoadRun(context.Background(), runID)
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished", r.Status)
	}
	if len(campaignIns) == 0 {
		t.Fatal("campaign never ran")
	}
	return exec, campaignIns
}

// requireEmpty asserts a hand-off field is PRESENT and "": absence means
// the edge did not map it, and the campaign prompt would render the raw
// "{{input.<field>}}" placeholder instead of nothing.
func requireEmpty(t *testing.T, in map[string]any, field string) {
	t.Helper()
	v, present := in[field]
	if !present {
		t.Errorf("campaign input lacks %q entirely — the prompt renders a raw {{input.%s}} placeholder", field, field)
		return
	}
	if s, _ := v.(string); s != "" || strings.Contains(s, "{{") {
		t.Errorf("campaign input %q = %v, want \"\"", field, v)
	}
}

// plan_review off (creds-unresolved / operator off) → the whole phase is
// bypassed and the campaign receives EMPTY hand-off fields, never
// placeholders.
func TestPlanReview_OffBypassesThePhase(t *testing.T) {
	exec, ins := runPlanReview(t, "e2e-plan-off", map[string]any{"plan_review": "off"}, false)
	for _, id := range []string{"plan", "plan_review", "plan_revise"} {
		if exec.wasCalled(id) {
			t.Errorf("off: %s ran — the phase must be bypassed whole", id)
		}
	}
	for _, f := range []string{"fail_log", "plan", "plan_critique", "plan_responses"} {
		requireEmpty(t, ins[0], f)
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
// with the revised plan + critique handed to the campaign on pass 1 and
// BLANKED on pass 2 (the back-edge wins over the re-applied forward
// mapping — passes 2+ read git log, not a stale plan).
func TestPlanReview_OnRunsAuthorPeerRevise(t *testing.T) {
	exec, ins := runPlanReview(t, "e2e-plan-on", map[string]any{"plan_review": "on"}, false)
	for _, id := range []string{"plan", "plan_review", "plan_revise", "campaign"} {
		if !exec.wasCalled(id) {
			t.Errorf("on: %s never ran", id)
		}
	}
	first := ins[0]
	if first["plan"] != "slice 1 only" {
		t.Errorf("pass 1 received plan %q, want the REVISED plan", first["plan"])
	}
	if first["plan_critique"] != "slice 2 duplicates a seam" {
		t.Errorf("pass 1 received critique %q", first["plan_critique"])
	}
	if first["plan_responses"] != "accepted: reuse the seam" {
		t.Errorf("pass 1 received responses %q", first["plan_responses"])
	}
	requireEmpty(t, first, "fail_log")

	if len(ins) < 2 {
		t.Fatal("continuation loop never re-entered the campaign")
	}
	second := ins[1]
	for _, f := range []string{"plan", "plan_critique", "plan_responses"} {
		requireEmpty(t, second, f)
	}
	if second["fail_log"] != "FAILLOG" {
		t.Errorf("pass 2 fail_log = %v, want the gate's log", second["fail_log"])
	}
}

// Peer skipped (the executor's `action: skip` route fired — simulated by
// the _skipped stamp) → the revise turn is bypassed and the UNREVISED
// plan reaches the campaign, with a present-and-empty critique.
func TestPlanReview_SkippedRoutesAroundRevise(t *testing.T) {
	exec, ins := runPlanReview(t, "e2e-plan-skip", map[string]any{
		"plan_review": "on", "plan_review_policy": "skip",
	}, true)
	if exec.wasCalled("plan_revise") {
		t.Error("skipped: plan_revise ran — a skipped review has nothing to challenge")
	}
	first := ins[0]
	if first["plan"] != "slice 1, slice 2" {
		t.Errorf("campaign received plan %q, want the AUTHOR's plan", first["plan"])
	}
	requireEmpty(t, first, "plan_critique")
	requireEmpty(t, first, "plan_responses")
	requireEmpty(t, first, "fail_log")
}
