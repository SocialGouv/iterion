package bots

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

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
