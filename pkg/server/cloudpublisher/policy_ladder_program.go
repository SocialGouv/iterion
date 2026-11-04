package cloudpublisher

import (
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/llmroute"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// composePolicyLadder is the adaptive-routing ladder's materialization
// (ADR-121 §1, slice 3): the HELD pairs in policy order, landed on the
// program through the SAME screen ApplyRunFallback applies, the operator's
// run-level chain first. It returns the stages it composed — nil when no pair
// is held, which is the "no ladder at all" answer.
//
// ONE composer for both surfaces. The launch composes onto the program it
// publishes and KEEPS the rungs: its derivations read the node fallbacks this
// call lands, and the computed stages ride the RunMessage beside the
// operator's chain (res.Ladder), flagged Policy so the preflight judges their
// spend. The credential preview composes to READ the same program and
// restores it (snapshotFallbackRoutes). What differs between them is the
// funding predicate the held pairs come through — doorServes on a sealed
// bundle, doorServesMetadata on the fills a preview predicts — never the
// rule: a surface judging a program this one did not compose is a view free
// to contradict the launch it describes.
//
// Both screens are ADVISORY: the runner re-applies them as the authority on
// its recompiled program (the publish serializes source, not IR). refused, if
// non-nil, receives each screen refusal; a read-only surface passes nil —
// the authoritative report is the runner's run_fallback_refused event.
//
// A CLAMPED-EMPTY trigger set is the "never switch" answer: no ladder at all.
// (The wire's On would otherwise round-trip empty into the chain DEFAULT set
// — the ceiling's forbidden triggers armed, the slice-3 review's M4.)
func composePolicyLadder(wf *ir.Workflow, view model.RouteView, routePolicy *store.RunLLMRoutePolicy, allowed map[string]bool, launchVars map[string]string, serves func(_, credential string) bool, refused func(string)) []ir.PolicyLadderStage {
	if wf == nil || routePolicy == nil || len(routePolicy.Triggers) == 0 {
		return nil
	}
	held := llmroute.HeldPairs(allowlistedPairs(routePolicy.PairOrder, allowed), serves)
	if len(held) == 0 {
		return nil
	}
	stages := make([]ir.PolicyLadderStage, 0, len(held))
	for _, pair := range held {
		if h, c, err := llmroute.ParsePair(pair); err == nil {
			stages = append(stages, ir.PolicyLadderStage{Harness: h, Credential: c, On: routePolicy.Triggers, CrossHarness: routePolicy.CrossHarness})
		}
	}
	report := func(refusals []string) {
		if refused == nil {
			return
		}
		for _, r := range refusals {
			refused(r)
		}
	}
	// The OPERATOR chain lands first (the same screen the runner applies):
	// the ladder's eligibility reads authored routes only, and the composed
	// program is what the derivations after this call must see.
	operatorIR := make([]ir.Fallback, 0, len(view.RunFallbacks))
	for _, fb := range view.RunFallbacks {
		operatorIR = append(operatorIR, ir.Fallback{Name: ir.RunFallbackName, Backend: fb.Backend, Model: fb.Model, Provider: fb.Provider})
	}
	// The publisher's sandbox reading is the workflow's own block only — the
	// runner's CLI override and deployment default are invisible here, and so
	// is the #1959 isolation floor: this screen has no resolved bot-source
	// tier to judge (it is also reached from a credential PREVIEW, which runs
	// before any source resolves). A team-authored `sandbox: none` therefore
	// reads UNSANDBOXED here and sandboxed at the runner, which widens the
	// same named asymmetry a CLI override already creates: this screen is
	// advisory, and the runner's run_fallback_refused event is the
	// authoritative one.
	sandboxed := runtime.WorkflowSandboxActive(wf, "", "")
	// The screens read the run's launch vars like the derivations read the
	// view: one reading of the run, all of it — the publisher disagreeing
	// with itself over a templated program is the parity the ADR-129 §4 twin
	// owes (ADR-129 §7).
	report(ir.ApplyRunFallback(wf, operatorIR, sandboxed, launchVars, func(n ir.LLMNode) string {
		ov := view.Overrides.ForNode(n.NodeID(), n.NodeKind())
		return ov.Backend
	}))
	report(ir.ApplyPolicyLadder(wf, stages, sandboxed, launchVars, func(n ir.LLMNode) string {
		ov := view.Overrides.ForNode(n.NodeID(), n.NodeKind())
		if ov.Model != "" {
			return ir.ExpandEnvWithDefault(ov.Model)
		}
		return ir.ExpandEnvWithDefault(n.GetLLMFields().Model)
	}, func(n ir.LLMNode) string {
		ov := view.Overrides.ForNode(n.NodeID(), n.NodeKind())
		return ov.Backend
	}, llmroute.ResolveClasses(routePolicy.ModelClasses)))
	return stages
}

// snapshotFallbackRoutes freezes the fallback list of every agent node and
// hands back the restore. ApplyRunFallback and ApplyPolicyLadder only APPEND,
// and only to an *ir.AgentNode, so putting the saved slice headers back
// leaves the program exactly as it was read: the elements under the saved
// length are never rewritten, and nothing beyond it is observable.
//
// That is what lets a READ-ONLY surface run composePolicyLadder — the
// credential preview judges the program the launch judges, and hands the
// derivations after it the authored one. The door probe (doorWantsFor)
// restores by stripping the Policy flag instead, the invariant its own
// comment states.
func snapshotFallbackRoutes(wf *ir.Workflow) func() {
	if wf == nil {
		return func() {}
	}
	saved := make(map[string][]ir.Fallback, len(wf.Nodes))
	for id, n := range wf.Nodes {
		if agent, ok := n.(*ir.AgentNode); ok {
			saved[id] = agent.Fallbacks
		}
	}
	return func() {
		for id, n := range wf.Nodes {
			agent, ok := n.(*ir.AgentNode)
			if !ok {
				continue
			}
			if routes, held := saved[id]; held {
				agent.Fallbacks = routes
			}
		}
	}
}
