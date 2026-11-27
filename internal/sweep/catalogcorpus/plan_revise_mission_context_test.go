package catalogcorpus

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The plan phase's author revises its own plan on a SECOND node, under
// `session: inherit_if_available` — best-effort by contract: the engine marks
// the session droppable and the retry re-sends the prompt without it
// (pkg/backend/model/executor_build_task.go, applySessionContinuity). So the
// mission the `plan` node rendered — the operator's launch values — reaches
// `plan_revise` only if a channel of THAT node renders it
// (docs/agents/bots/input-reaches-the-model.md). It travels as one typed
// `mission_context` field the plan_gate edge fills from the same vars, which
// is what keeps the seven `plan_revise_user` prompts one text.
//
// The two guards below are one pair and neither is sufficient alone: the
// first proves the mission arrives and renders, per bundle, through the real
// resolver; the second holds the seven prompts to ONE text, so the block
// cannot be dropped from one bundle while six keep it.

// planRevisePromptNode is the node this pair is about, and the edge target
// whose incoming `with` mappings build its input.
const planRevisePromptNode = "plan_revise"

// TestThePlanRevisePromptsAreOneText is the drift guard the typed field buys:
// the per-bundle difference lives entirely in the plan_gate edge's
// `mission_context` value, so the seven prompts themselves are ONE text. The
// class is discovered from the compiled bundles rather than from
// planPhaseBots — and the two sets are compared, so the hand-written contract
// list cannot go stale on either side (a bundle that grows the phase without
// joining the list, or a list naming a bundle that lost it).
//
// Byte identity, not similarity: this is the invariant that makes the pair
// above provable for seven bundles from one reading of the text.
func TestThePlanRevisePromptsAreOneText(t *testing.T) {
	bodies := map[string]string{}
	for _, sw := range shippedWorkflows(t) {
		if _, ok := sw.wf.Nodes[planRevisePromptNode]; !ok {
			continue
		}
		node, isLLM := sw.wf.Nodes[planRevisePromptNode].(ir.LLMNode)
		if !isLLM {
			t.Errorf("%s%s is %T, not an LLM node — this class is about what a model reads", sw.prefix, planRevisePromptNode, sw.wf.Nodes[planRevisePromptNode])
			continue
		}
		usr := sw.wf.Prompts[node.GetLLMFields().UserPrompt]
		if usr == nil {
			t.Errorf("%s%s declares no user prompt — the half every value it shows renders in", sw.prefix, planRevisePromptNode)
			continue
		}
		bodies[sw.prefix] = usr.Body
	}

	// The discovered class and the declared contract list are the same set.
	discovered := make([]string, 0, len(bodies))
	for prefix := range bodies {
		discovered = append(discovered, strings.TrimSuffix(prefix, "/"))
	}
	sort.Strings(discovered)
	declared := slices.Clone(planPhaseBots)
	sort.Strings(declared)
	if !slices.Equal(discovered, declared) {
		t.Errorf("the bundles carrying a %q node are %v, planPhaseBots says %v — one of the two is stale, and every guard keyed on the list answers for the wrong set until they agree",
			planRevisePromptNode, discovered, declared)
	}
	if len(bodies) < 2 {
		t.Fatalf("only %d %q prompt(s) found: a drift guard over fewer than two texts proves nothing", len(bodies), planRevisePromptNode)
	}

	// One text, byte for byte. The reference is the lexicographically first
	// prefix so the report is stable whatever the walk order.
	prefixes := make([]string, 0, len(bodies))
	for prefix := range bodies {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)
	ref := prefixes[0]
	for _, prefix := range prefixes[1:] {
		if bodies[prefix] == bodies[ref] {
			continue
		}
		t.Errorf("%s%s's user prompt is not %s%s's, byte for byte — the seven are ONE text so the mission block is provable for all of them from one reading; the per-bundle difference belongs in the plan_gate edge's mission_context value.\n%s",
			prefix, planRevisePromptNode, ref, planRevisePromptNode, firstDivergence(bodies[ref], bodies[prefix]))
	}

	// And the one text carries the mission block: a drift guard over seven
	// identical prompts stays green if all seven drop it together.
	if n := strings.Count(bodies[ref], "{{input.mission_context}}"); n != 1 {
		t.Errorf("the shared %q prompt renders {{input.mission_context}} %d time(s), want exactly 1 — without it the typed field is plumbing that delivers nothing, and the seven agree on a prompt that loses the mission", planRevisePromptNode, n)
	}
}

// firstDivergence reports where two texts stop agreeing, by line, so the
// drift guard's message points at the edit instead of printing two prompts.
func firstDivergence(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var wl, gl string
		if i < len(w) {
			wl = w[i]
		}
		if i < len(g) {
			gl = g[i]
		}
		if wl == gl {
			continue
		}
		return fmt.Sprintf("  first divergence at line %d:\n    reference: %q\n    this one:  %q", i+1, wl, gl)
	}
	return "  the texts agree line by line but differ in bytes (a trailing newline, a non-breaking space)"
}
