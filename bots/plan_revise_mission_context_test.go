package bots

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
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

// TestEveryMissionVarThePlanNodeRendersReachesPlanRevise is the class guard.
// The subject is discovered from the bundle, never listed: the mission is
// whatever `{{vars.*}}` the `plan` node's OWN channels render, and every one
// of them must come out of `plan_revise`'s own channels on EVERY route that
// enters it — a route that leaves the key alone delivers the literal
// `{{input.mission_context}}` placeholder to the model, not the mission.
//
// The render is the engine's: model.TemplateResolver over the edge's `with`
// value and then over the node's prompts, with a delimited marker per var.
// A launch carries no session, so nothing but these texts reaches the author.
func TestEveryMissionVarThePlanNodeRendersReachesPlanRevise(t *testing.T) {
	for _, bot := range planPhaseBots {
		t.Run(bot, func(t *testing.T) {
			wf := compileBotFile(t, filepath.Join(bot, "main.bot"))

			owed := missionVarsRenderedBy(t, wf, "plan")
			if len(owed) == 0 {
				t.Fatalf("%s: the plan node's own channels render no {{vars.*}} at all — this guard's subject is gone, or the prompt walk stopped seeing it", bot)
			}

			routes := incomingRoutes(wf, planRevisePromptNode)
			if len(routes) == 0 {
				t.Fatalf("%s: no edge reaches %q — its input is built from the incoming `with` mappings, so the node would receive nothing", bot, planRevisePromptNode)
			}

			for _, e := range routes {
				rendered := renderNodeChannels(t, wf, planRevisePromptNode, inputFromEdge(wf, e))
				for _, name := range owed {
					if strings.Contains(rendered, marker(name)) {
						continue
					}
					t.Errorf("%s: the plan node renders {{vars.%s}} but no channel of %q delivers it on the route %s — on a launch whose best-effort session did not survive, the author revises a plan whose mission it cannot see. Carry it in the edge's `mission_context` value and render {{input.mission_context}} in the node's own prompt (docs/agents/bots/input-reaches-the-model.md)",
						bot, name, planRevisePromptNode, edgeRoute(e))
				}
			}
			t.Logf("%s: %d mission var(s) %v checked on %d route(s)", bot, len(owed), owed, len(routes))
		})
	}
}

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

// missionVarsRenderedBy is the mission as the bundle itself defines it: the
// `{{vars.*}}` names the node's own channels render, read from the compiled
// references rather than from the prompt text, so a padded `{{ vars.x }}` or
// a var moved from the user prompt to the system prompt counts the same.
func missionVarsRenderedBy(t *testing.T, wf *ir.Workflow, nodeID string) []string {
	t.Helper()
	node, isLLM := wf.Nodes[nodeID].(ir.LLMNode)
	if !isLLM {
		t.Fatalf("%q is %T, want an LLM node", nodeID, wf.Nodes[nodeID])
	}
	f := node.GetLLMFields()
	seen := map[string]bool{}
	for _, name := range []string{f.SystemPrompt, f.UserPrompt} {
		p := wf.Prompts[name]
		if p == nil {
			continue
		}
		for _, ref := range p.TemplateRefs {
			if ref == nil || ref.Kind != ir.RefVars || len(ref.Path) == 0 {
				continue
			}
			seen[ref.Path[0]] = true
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// incomingRoutes are the edges that build a node's input. An edge with no
// `with` mapping contributes nothing, which is precisely a route that
// delivers no mission, so it is kept rather than skipped.
func incomingRoutes(wf *ir.Workflow, nodeID string) []*ir.Edge {
	var out []*ir.Edge
	for _, e := range wf.Edges {
		if e != nil && e.To == nodeID {
			out = append(out, e)
		}
	}
	return out
}

// inputFromEdge resolves one edge's `with` mapping the way the runtime does
// for a launch: every declared var holds its own marker, and a reference the
// launch cannot answer (another node's output) stays as written — the mission
// is the vars, and nothing else is claimed here.
func inputFromEdge(wf *ir.Workflow, e *ir.Edge) map[string]any {
	vars := make(map[string]any, len(wf.Vars))
	for name := range wf.Vars {
		vars[name] = marker(name)
	}
	resolver := &model.TemplateResolver{Vars: vars}
	input := make(map[string]any, len(e.With))
	for _, dm := range e.With {
		if dm == nil {
			continue
		}
		input[dm.Key] = resolver.Resolve(dm.Raw, nil, nil)
	}
	return input
}

// renderNodeChannels renders the node's own system and user prompts against
// the input an edge built — the one reading of "a channel of this node
// renders it" the ratchet and the prose guard also use.
func renderNodeChannels(t *testing.T, wf *ir.Workflow, nodeID string, input map[string]any) string {
	t.Helper()
	node, isLLM := wf.Nodes[nodeID].(ir.LLMNode)
	if !isLLM {
		t.Fatalf("%q is %T, want an LLM node", nodeID, wf.Nodes[nodeID])
	}
	f := node.GetLLMFields()
	resolver := &model.TemplateResolver{}
	var b strings.Builder
	for _, name := range []string{f.SystemPrompt, f.UserPrompt} {
		p := wf.Prompts[name]
		if p == nil {
			continue
		}
		b.WriteString(resolver.Resolve(p.Body, input, nil))
		b.WriteString("\n")
	}
	return b.String()
}

// edgeRoute names an edge the way its author wrote it, guard included: two
// edges between the same pair differ only by their condition.
func edgeRoute(e *ir.Edge) string {
	route := e.From + " -> " + e.To
	switch {
	case e.ExpressionSrc != "":
		route += " when " + e.ExpressionSrc
	case e.Condition != "" && e.Negated:
		route += " when not " + e.Condition
	case e.Condition != "":
		route += " when " + e.Condition
	case e.IsElse:
		route += " else"
	}
	return route
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
