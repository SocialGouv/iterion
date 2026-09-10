package bots

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// compilePlanPhaseBot compiles one campaign bot for the plan-phase guards,
// failing the test on a compile error (a bot that stops compiling must not
// drop out of a class-level guard silently).
func compilePlanPhaseBot(t *testing.T, bot string) *ir.Workflow {
	t.Helper()
	path := filepath.Join(bot, "main.bot")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	cr := ir.Compile(parser.Parse(path, string(src)).File)
	if cr.HasErrors() {
		t.Fatalf("%s does not compile: %+v", path, cr.Diagnostics)
	}
	return cr.Workflow
}

// computeExprRaw returns the raw source of one field expression of a
// compute node ("" when the node or the field is absent).
func computeExprRaw(wf *ir.Workflow, nodeID, key string) string {
	n, ok := wf.Nodes[nodeID].(*ir.ComputeNode)
	if !ok {
		return ""
	}
	for _, ex := range n.Exprs {
		if ex.Key == key {
			return ex.Raw
		}
	}
	return ""
}

// mappingOf returns the data mapping for key on an edge (nil when unmapped).
func mappingOf(e *ir.Edge, key string) *ir.DataMapping {
	for _, m := range e.With {
		if m.Key == key {
			return m
		}
	}
	return nil
}

// TestPlanPhaseGatesTheReviewNotThePhase pins the split every campaign bot
// carries: the plan phase — the AUTHORED plan the campaign implements from
// — runs by default on every deployment, and `plan_phase` (on|off, default
// on) is its only switch; `plan_review` gates ONLY the cross-model peer
// review of that plan. `ResolvePlanReview` returns off on every
// single-provider deployment, so a `plan_topology` keyed on plan_review
// silently skipped planning for the commonest setup there is (a desktop
// with one Claude subscription) — and an operator turning the REVIEW off
// to save a peer pass lost the author along with the missing reviewer.
//
// Per bot: the phase switch is declared with the right default; the entry
// gate reads plan_phase and never plan_review; the review gate sits AFTER
// the plan is authored; and the review-off edge hands the campaign the
// author's plan, stamped as unreviewed (never an empty plan).
func TestPlanPhaseGatesTheReviewNotThePhase(t *testing.T) {
	for _, bot := range planPhaseBots {
		t.Run(bot, func(t *testing.T) {
			wf := compilePlanPhaseBot(t, bot)

			v, ok := wf.Vars["plan_phase"]
			if !ok {
				t.Fatalf("%s: var plan_phase not declared — the plan phase has no switch of its own (plan_review must gate only the review)", bot)
			}
			if got, _ := v.Default.(string); got != "on" {
				t.Errorf("%s: plan_phase default = %q, want \"on\" (planning runs by default, on every deployment)", bot, got)
			}
			if len(v.EnumValues) != 2 {
				t.Errorf("%s: plan_phase lacks its [enum: on, off] — a typo'd --var would silently select a value", bot)
			}

			doPlan := computeExprRaw(wf, "plan_topology", "do_plan")
			if doPlan == "" {
				t.Fatalf("%s: plan_topology has no do_plan expression", bot)
			}
			if !strings.Contains(doPlan, "vars.plan_phase") {
				t.Errorf("%s: plan_topology.do_plan = %q does not read vars.plan_phase", bot, doPlan)
			}
			if strings.Contains(doPlan, "plan_review") {
				t.Errorf("%s: plan_topology.do_plan = %q reads plan_review — the REVIEW switch is gating the whole PHASE (single-provider hosts never plan)", bot, doPlan)
			}

			doReview := computeExprRaw(wf, "plan_review_topology", "do_review")
			if doReview == "" {
				t.Fatalf("%s: plan_review_topology (the peer-review gate after the plan is authored) is missing or has no do_review expression", bot)
			}
			if !strings.Contains(doReview, "vars.plan_review") {
				t.Errorf("%s: plan_review_topology.do_review = %q does not read vars.plan_review", bot, doReview)
			}

			// The authored plan flows into the review gate, and the gate
			// forks on do_review: on → the peer; off → onward WITHOUT
			// any LLM in between, carrying the author's plan and a
			// non-blank provenance stamp.
			var planToGate, gateToPeer, gateOff *ir.Edge
			for _, e := range wf.Edges {
				switch {
				case e.From == "plan" && e.To == "plan_review_topology":
					planToGate = e
				case e.From == "plan_review_topology" && e.To == "plan_review" && e.Condition == "do_review" && !e.Negated:
					gateToPeer = e
				case e.From == "plan_review_topology" && e.Condition == "do_review" && e.Negated:
					gateOff = e
				}
			}
			if planToGate == nil {
				t.Errorf("%s: no edge plan -> plan_review_topology", bot)
			}
			if gateToPeer == nil {
				t.Errorf("%s: no edge plan_review_topology -> plan_review when do_review", bot)
			}
			if gateOff == nil {
				t.Fatalf("%s: no edge plan_review_topology -> … when not do_review — a run without a second model family has no path from the authored plan to the campaign", bot)
			}
			switch wf.Nodes[gateOff.To].(type) {
			case *ir.AgentNode, *ir.JudgeNode:
				if gateOff.To != "campaign" {
					t.Errorf("%s: the review-off edge targets LLM node %q — the unreviewed plan must reach the campaign through deterministic nodes only", bot, gateOff.To)
				}
			}
			plan := mappingOf(gateOff, "plan")
			if plan == nil || plan.Raw != "{{outputs.plan.plan}}" {
				t.Errorf("%s: the review-off edge maps plan = %v, want {{outputs.plan.plan}} (the AUTHOR's plan must reach the campaign)", bot, plan)
			}
			prov := mappingOf(gateOff, "plan_provenance")
			if prov == nil || strings.TrimSpace(prov.Raw) == "" || len(prov.Refs) != 0 {
				t.Errorf("%s: the review-off edge carries no literal plan_provenance stamp — the campaign cannot tell an unreviewed plan from a peer-reviewed one", bot)
			} else if !strings.Contains(strings.ToLower(prov.Raw), "not peer-reviewed") {
				t.Errorf("%s: plan_provenance stamp %q does not say the plan is NOT peer-reviewed", bot, prov.Raw)
			}

			// And no edge out of the entry gate routes on the review switch.
			for _, e := range wf.Edges {
				if e.From == "plan_topology" && e.Condition != "" && e.Condition != "do_plan" {
					t.Errorf("%s: plan_topology -> %s routes on %q — the entry gate must fork on do_plan only", bot, e.To, e.Condition)
				}
			}

			schema, ok := wf.Schemas["campaign_input"]
			if !ok {
				t.Fatal("campaign_input schema missing")
			}
			hasProv := false
			for _, f := range schema.Fields {
				if f.Name == "plan_provenance" {
					hasProv = true
				}
			}
			if !hasProv {
				t.Errorf("%s: campaign_input has no plan_provenance field — the stamp has nowhere to land", bot)
			}
		})
	}
}

// repoRequiringCampaignBots are the campaign bots whose mission needs an
// EXISTING checkout: every one of them starts with an LLM plan node that
// would otherwise be paid against nothing on a misconfigured launch (a
// `--bot` launch carrying only pr_url attaches no repository). app-dev is
// the deliberate exception: it starts from an empty directory and
// `git init`s from slice 0.
var repoRequiringCampaignBots = []string{
	"feature-dev",
	"branch-improve-loop",
	"whole-improve-loop",
	"feature-gap-fill",
	"test-coverage",
	"e2e-coverage",
}

// TestCampaignWorkspacePrecondition pins the deterministic precondition
// ahead of the first LLM node: `workspace_probe` is the run's ENTRY, a tool
// node (no LLM, no budget) refusing with the typed code WORKSPACE_NOT_A_REPO
// when workspace_dir is absent / not a git repository — and, for a bot whose
// mission is anchored on a base ref, when that base is not reachable from
// HEAD. The verdict lands on the node's output and routes to the named
// `workspace_not_a_repo` fail node, which stamps the code on the RUN
// (bots/typed_fail_test.go owns that half).
func TestCampaignWorkspacePrecondition(t *testing.T) {
	for _, bot := range repoRequiringCampaignBots {
		t.Run(bot, func(t *testing.T) {
			wf := compilePlanPhaseBot(t, bot)
			if wf.Entry != "workspace_probe" {
				t.Errorf("%s: entry = %q, want workspace_probe — an LLM node can run before anything checks there is a repository", bot, wf.Entry)
			}
			probe, ok := wf.Nodes["workspace_probe"].(*ir.ToolNode)
			if !ok {
				t.Fatalf("%s: workspace_probe is %T, want *ir.ToolNode (deterministic, no LLM)", bot, wf.Nodes["workspace_probe"])
			}
			for _, want := range []string{"{{vars.workspace_dir}}", "rev-parse", "--git-dir", "WORKSPACE_NOT_A_REPO"} {
				if !strings.Contains(probe.Command, want) {
					t.Errorf("%s: workspace_probe command lacks %q", bot, want)
				}
			}
			if _, hasBase := wf.Vars["base_ref"]; hasBase {
				for _, want := range []string{"{{vars.base_ref}}", "merge-base"} {
					if !strings.Contains(probe.Command, want) {
						t.Errorf("%s: declares base_ref but workspace_probe does not check it is reachable (%q missing) — a plan authored against a range that does not exist", bot, want)
					}
				}
			}
			var toGate, toFail bool
			for _, e := range wf.Edges {
				if e.From != "workspace_probe" {
					continue
				}
				switch {
				case e.To == "plan_topology" && e.Condition == "ok" && !e.Negated:
					toGate = true
				case e.To == "workspace_not_a_repo" && e.Condition == "ok" && e.Negated:
					toFail = true
				default:
					t.Errorf("%s: unexpected edge workspace_probe -> %s (when %q negated=%v)", bot, e.To, e.Condition, e.Negated)
				}
			}
			if !toGate {
				t.Errorf("%s: no edge workspace_probe -> plan_topology when ok", bot)
			}
			if !toFail {
				t.Errorf("%s: no edge workspace_probe -> workspace_not_a_repo when not ok — a refused workspace would not fail the run", bot)
			}
			out, ok := wf.Schemas["workspace_probe_state"]
			if !ok {
				t.Fatalf("%s: workspace_probe_state schema missing", bot)
			}
			fields := map[string]bool{}
			for _, f := range out.Fields {
				fields[f.Name] = true
			}
			for _, want := range []string{"ok", "code", "reason"} {
				if !fields[want] {
					t.Errorf("%s: workspace_probe_state lacks field %q (the typed verdict must be visible on the node output)", bot, want)
				}
			}
		})
	}
	// app-dev git-inits an empty workspace itself: a repository precondition
	// there would refuse the greenfield run it exists for.
	t.Run("app-dev", func(t *testing.T) {
		wf := compilePlanPhaseBot(t, "app-dev")
		if _, ok := wf.Nodes["workspace_probe"]; ok {
			t.Error("app-dev carries a workspace_probe — it must accept an empty, non-git workspace (greenfield)")
		}
	})
}
