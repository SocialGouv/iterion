package model

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The dispatch-view witnesses (ADR-129 §4): the runner-side reading answers
// for the run's OWN inputs. A launch value this process cannot reproduce —
// an engine-supplied name anywhere in its text — keeps the name undecided,
// never the declared default's answer; a literal value over an
// env-consulting default is the run's decision; a nil workflow reads as the
// inert view; a launch value nobody pinned narrows nothing; and the wire
// walk arms on PRIMARY routes only, whatever the run's fallback chain says.

// An override whose text consults an engine-supplied name (here: one
// harmless note leaf of a json document naming ${PROJECT_DIR}) is skipped
// whole — and must not leave the declared default standing in the var's
// place, DECIDED: the run's own document resolves a different route, and a
// gate that certified the default's certifies a route the run never takes.
func TestRouteViewForDispatch_EngineNamedOverrideStaysUndecided(t *testing.T) {
	wf := &ir.Workflow{
		Vars: map[string]*ir.Var{
			"cfg": {Name: "cfg", Type: ir.VarJSON, HasDefault: true, Default: `{"backend":"claw","model":"openai_compatible/gw-1"}`},
		},
		Nodes: map[string]ir.Node{
			"impl": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "impl"}, LLMFields: ir.LLMFields{
				Backend: "{{vars.cfg.backend}}", Model: "{{vars.cfg.model}}",
			}},
		},
	}
	msgVars := map[string]any{"cfg": `{"backend":"claude_code","note":"${PROJECT_DIR}"}`}
	view := RouteViewForDispatch[any](wf, ModelOverrides{}, nil, msgVars)
	if got := view.field("{{vars.cfg.backend}}"); got != "{{vars.cfg.backend}}" {
		t.Errorf("the dispatch view read backend %q for a run whose own document resolves claude_code — the engine-named leaf must keep the whole name undecided, not let the default stand in decided", got)
	}
	res := EffectiveProviders(view, knownForTest)
	if res.OnlyEnvFunded() {
		t.Errorf("OnlyEnvFunded = true for a run whose own cfg resolves backend claude_code — the wire route was never seen")
	}
	// The scalar twin: the same rule one type up.
	wf2 := &ir.Workflow{
		Vars: map[string]*ir.Var{
			"b": {Name: "b", Type: ir.VarString, HasDefault: true, Default: "claw"},
		},
		Nodes: map[string]ir.Node{
			"impl": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "impl"}, LLMFields: ir.LLMFields{Backend: "{{vars.b}}"}},
		},
	}
	view2 := RouteViewForDispatch[any](wf2, ModelOverrides{}, nil, map[string]any{"b": "${PROJECT_DIR}/scratch"})
	if got := view2.field("{{vars.b}}"); got != "{{vars.b}}" {
		t.Errorf("scalar twin: the dispatch view read %q for an override the engine expands to its own workDir path", got)
	}
}

// A literal value arriving on the run message is the run's own decision, over
// a default whose text consults the environment: the name is not late, and
// the field resolves the way dispatch will. The env-consulting DEFAULT alone
// stays late — the reading process's word, not the run's.
func TestRouteViewForDispatch_LiteralMsgValueOverEnvDefaultDecides(t *testing.T) {
	t.Setenv("E2RV_DISPATCH_LIT", "")
	wf := &ir.Workflow{
		Vars: map[string]*ir.Var{
			"b": {Name: "b", Type: ir.VarString, HasDefault: true, Default: "${E2RV_DISPATCH_LIT:-claw}"},
		},
		Nodes: map[string]ir.Node{
			"impl": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "impl"}, LLMFields: ir.LLMFields{Backend: "{{vars.b}}"}},
		},
	}
	view := RouteViewForDispatch[any](wf, ModelOverrides{}, nil, map[string]any{"b": "claw"})
	if view.LateVars["b"] {
		t.Errorf("late[b] = true for a literal message value — the override layer is the run's final answer for the name")
	}
	if got := view.field("{{vars.b}}"); got != "claw" {
		t.Errorf("field = %q, want claw — the run dispatches the message's literal value", got)
	}
	// Control: the default alone stays late and undecided.
	view2 := RouteViewForDispatch[any](wf, ModelOverrides{}, nil, nil)
	if !view2.LateVars["b"] {
		t.Errorf("control late[b] = false for the env-consulting default alone — the environment decided it")
	}
	if got := view2.field("{{vars.b}}"); got != "{{vars.b}}" {
		t.Errorf("control field = %q, want the raw template — a late name stays as written", got)
	}
}

// The exported constructor honours the sibling's contract: a nil workflow is
// the inert view, not a panic.
func TestRouteViewForDispatch_NilWorkflowIsTheInertView(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("RouteViewForDispatch panicked on a nil workflow: %v", r)
		}
	}()
	v := RouteViewForDispatch[string](nil, ModelOverrides{}, nil, nil)
	if v.Workflow != nil || v.Vars != nil || v.LateVars != nil {
		t.Errorf("RouteViewForDispatch(nil, …) = %+v, want the inert view", v)
	}
}

// A launch value narrows a routing field only when the lane pinned its name
// (ADR-129 §4 rule 2): the webhook lanes fill vars from the delivery
// payload, so an unpinned value is late whoever wrote it.
// Mutant: the trusted-marking loop of RouteViewForLaunch deleted → the
// unpinned half reds on its first assertion, the trusted half on its second.
func TestRouteViewForLaunch_LaunchValueNarrowsOnlyWhenTrusted(t *testing.T) {
	wf := &ir.Workflow{
		Vars: map[string]*ir.Var{
			"m": {Name: "m", Type: ir.VarString, HasDefault: true, Default: "openai_compatible/gw-1"},
		},
		Nodes: map[string]ir.Node{
			"impl": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "impl"}, LLMFields: ir.LLMFields{Backend: "claw", Model: "{{vars.m}}"}},
		},
	}
	lateView := RouteViewForLaunch[string](wf, ModelOverrides{}, nil, map[string]string{"m": "openai_compatible/gw-2"}, nil)
	if got := lateView.field("{{vars.m}}"); got != "{{vars.m}}" {
		t.Errorf("untrusted launch value resolved the field to %q, want the raw template (late)", got)
	}
	trustedView := RouteViewForLaunch[string](wf, ModelOverrides{}, nil, map[string]string{"m": "openai_compatible/gw-2"}, []string{"m"})
	if got := trustedView.field("{{vars.m}}"); got != "openai_compatible/gw-2" {
		t.Errorf("trusted launch value left the field at %q, want the launch value", got)
	}
	res := EffectiveProviders(lateView, knownForTest)
	if res.OnlyEnvFunded() {
		t.Errorf("OnlyEnvFunded = true on an unpinned launch value — the payload chose the gateway the gate certified")
	}
}

// The wire walk arms on PRIMARY routes only: the run's operator fallback
// chain reaches the walk in no field, and a chain naming the wire must not
// arm the park — the rescue fires on a failure the mid-run guard already
// refuses. Pins the doctrine the usage-cap pre-flight's call site names.
func TestAnthropicWireRoutes_RunFallbacksNeverArmTheWalk(t *testing.T) {
	wf := &ir.Workflow{
		Nodes: map[string]ir.Node{
			"impl": clawAgent("impl", "", "openai/gpt-6"),
		},
	}
	onWire := RouteView{Workflow: wf, RunFallbacks: []FallbackEntry{
		{Backend: "claude_code", Model: "claude-opus-5-5"},
		{Backend: "claw", Provider: "anthropic", Model: "claude-opus-5-5"},
	}}
	if routes := AnthropicWireRoutes(onWire); len(routes) != 0 {
		t.Errorf("the wire walk answered %d route(s) with a claude_code operator chain on the view — PRIMARY routes only; a rescue route must not arm the guard", len(routes))
	}
}
