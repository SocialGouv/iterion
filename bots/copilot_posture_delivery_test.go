package bots

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/expr"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// Copi's posture is a launch var (`mode`) that travels seed -> turn_state ->
// copi. Before #2555 it reached the register and stopped there: copi_input
// declared no `mode`, so the ONLY edge into copi carried none and no prompt
// rendered one — the operator's `mode=debug` launch was invisible to the model
// on turn 1, and Terra re-derived a posture from the conversation instead.
// `copi_input` is copi's OWN input schema (#2562) precisely so a field can be
// added or dropped there, which means a new field needs the schema AND the
// edge mapping in the same change (docs/agents/bots/input-reaches-the-model.md).
//
// This guard walks the real turn-1 path stage by stage, each stage read from
// the compiled bundle and evaluated by the engine's own evaluator and
// resolver: the seed's expression, turn_state's expression, the edge's
// mapping, then copi's own channels. Drop any one of the four and it reddens.

// copilotPostureNode and copilotPostureField name what this guard is about.
const (
	copilotPostureNode  = "copi"
	copilotPostureField = "mode"
)

func TestCopilotTurnOnePostureReachesTheModel(t *testing.T) {
	wf := compileBotFile(t, "copilot/main.bot")

	// Jo's scenario, literally: a `mode=design` launch whose turn-1 rendered
	// prompt carries `design`. Read on the USER prompt alone — copi_system
	// lists the three postures by name, so the whole-prompt reading would be
	// green on its own prose.
	_, usr := renderCopilotTurnOne(t, wf, "design")
	if !strings.Contains(usr, "design") {
		t.Errorf("a mode=design launch renders a turn-1 user prompt that never says `design` — the operator's posture does not reach the model on the turn they chose it for:\n%s", usr)
	}

	// And the anti-tautology: a value no prompt could carry on its own. This
	// is the assertion that cannot be satisfied by the prompt's own prose,
	// and it reads BOTH channels, since either would deliver.
	sentinel := marker(copilotPostureField)
	sys, usr := renderCopilotTurnOne(t, wf, sentinel)
	if !strings.Contains(sys, sentinel) && !strings.Contains(usr, sentinel) {
		t.Errorf("the launch posture does not reach %q on turn 1: neither of its own prompts renders the value the launch supplied. The seed, turn_state, the single edge and the prompt are the four stages — one of them drops it (docs/agents/bots/input-reaches-the-model.md)", copilotPostureNode)
	}
}

// renderCopilotTurnOne is the turn-1 path, stage by stage, with mode as the
// launch supplied it. Nothing here re-spells a derivation: every stage is the
// bundle's own text, run by the engine's own evaluator / resolver.
func renderCopilotTurnOne(t *testing.T, wf *ir.Workflow, launchMode string) (system, user string) {
	t.Helper()

	// 1. the entry projection: `seed` fills the register from vars.
	seeded := evalComputeField(t, wf, "seed", copilotPostureField, &expr.Context{
		Vars: onePath(copilotPostureField, launchMode),
	})

	// 2. the register the whole graph reads: turn_state restates it.
	carried := evalComputeField(t, wf, "turn_state", copilotPostureField, &expr.Context{
		Input: onePath(copilotPostureField, seeded),
	})

	// 3. the edge into copi — the ONLY one, which is why dropping the field
	//    there makes it inert on every route rather than on one.
	var into []*ir.Edge
	for _, e := range wf.Edges {
		if e != nil && e.To == copilotPostureNode {
			into = append(into, e)
		}
	}
	if len(into) != 1 {
		t.Fatalf("%d edge(s) reach %q, want exactly 1 — this guard walks THE route into it, and the bundle's own comment rests on there being one", len(into), copilotPostureNode)
	}
	resolver := &model.TemplateResolver{}
	td := &model.TemplateData{
		Outputs: map[string]map[string]any{"turn_state": {copilotPostureField: carried}},
		Nodes:   wf.Nodes,
	}
	input := map[string]any{}
	for _, dm := range into[0].With {
		if dm != nil {
			input[dm.Key] = resolver.Resolve(dm.Raw, nil, td)
		}
	}

	// 4. copi's own channels: what the model actually reads.
	node, isLLM := wf.Nodes[copilotPostureNode].(ir.LLMNode)
	if !isLLM {
		t.Fatalf("%q is %T, want an LLM node", copilotPostureNode, wf.Nodes[copilotPostureNode])
	}
	f := node.GetLLMFields()
	render := func(name string) string {
		p := wf.Prompts[name]
		if p == nil {
			return ""
		}
		return resolver.Resolve(p.Body, input, td)
	}
	return render(f.SystemPrompt), render(f.UserPrompt)
}

// evalComputeField runs one compute node's expression for one output field,
// read from the compiled bundle and evaluated by pkg/dsl/expr — the engine's
// own evaluator — so a changed derivation is followed rather than restated.
func evalComputeField(t *testing.T, wf *ir.Workflow, nodeID, field string, ctx *expr.Context) string {
	t.Helper()
	raw := computeExprRaw(wf, nodeID, field)
	if raw == "" {
		t.Fatalf("compute %q no longer emits %q: the posture register is broken upstream of the model", nodeID, field)
	}
	ast, err := expr.Parse(raw)
	if err != nil {
		t.Fatalf("%s.%s does not parse: %v (source %q)", nodeID, field, err, raw)
	}
	v, err := ast.Eval(ctx)
	if err != nil {
		t.Fatalf("%s.%s does not evaluate: %v (source %q)", nodeID, field, err, raw)
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("%s.%s is %T %v, want a string (source %q)", nodeID, field, v, v, raw)
	}
	return s
}

// onePath answers one single-segment namespace path and nothing else, so an
// expression reading another name sees the absence the runtime would give it.
func onePath(name, value string) func([]string) any {
	return func(p []string) any {
		if len(p) == 1 && p[0] == name {
			return value
		}
		return nil
	}
}
