package model

import (
	"maps"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// The RouteView witnesses (ADR-129 §4): the launch-time walk reads the
// run's vars by PROVENANCE — a declared default whose value the run itself
// decided narrows the gate exactly as dispatch resolves it; a name the
// reading process's own environment decided (late) is as opaque as the raw
// `${…}` it came from; a launch value nobody pinned narrows nothing; and a
// `fallbacks:` route whose `when:` the vars answer FALSE leaves the walk,
// while a gate they cannot answer keeps it.

func clawAgent(id, provider, mdl string) *ir.AgentNode {
	return &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: id},
		LLMFields: ir.LLMFields{Backend: "claw", Provider: provider, Model: mdl},
	}
}

func gatewayVarProgram(def string) (*ir.Workflow, map[string]*ir.Var) {
	vars := map[string]*ir.Var{
		"m": {Name: "m", Type: ir.VarString, HasDefault: true, Default: def},
	}
	wf := &ir.Workflow{
		Vars: vars,
		Nodes: map[string]ir.Node{
			"impl": clawAgent("impl", "", "{{vars.m}}"),
		},
	}
	return wf, vars
}

// The `when:` gate joins the reading: a gated zai rescue whose var the run
// decided OFF is not a path the run may take, so the walk stops attributing
// z.ai spend to it. With no vars at all the route STAYS — a gate the view
// cannot answer keeps the route, and the walk never drops a provider the
// run might spend.
// Mutant: the FallbackWhenActive call deleted from the fallbacks loop → the
// gated zai returns → this test reds on the first assertion.
func TestEffectiveProviders_GatedRouteLeavesTheWalk(t *testing.T) {
	gated := func() *ir.Workflow {
		return &ir.Workflow{
			Vars: map[string]*ir.Var{
				"g": {Name: "g", Type: ir.VarString, HasDefault: true, Default: "on"},
			},
			Nodes: map[string]ir.Node{
				"impl": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "impl"}, Fallbacks: []ir.Fallback{
					{Name: "glm_rescue", Provider: "zai", When: "vars.g == 'on'"},
				}, LLMFields: ir.LLMFields{Backend: "claw", Model: "openai_compatible/gw/m"}},
			},
		}
	}
	// The vars answer OFF: the rescue leaves the walk.
	wf := gated()
	wf.Vars["g"].Default = "off"
	res := EffectiveProviders(RouteViewForLaunch[string](wf, ModelOverrides{}, nil, nil, nil), knownForTest)
	if slices.Contains(res.Providers, "zai") {
		t.Errorf("Providers = %v, want no zai — the run's own vars deactivated the rescue", res.Providers)
	}
	// No vars to answer the gate: the route stays (conservative).
	res = EffectiveProviders(RouteView{Workflow: gated(), Overrides: ModelOverrides{}}, knownForTest)
	if !slices.Contains(res.Providers, "zai") {
		t.Errorf("Providers = %v, want zai — an unanswered gate keeps the route", res.Providers)
	}
}

// The O1 witness: a var whose default consults the environment is LATE —
// its value is the reading process's word, not the run's — so the templated
// model stays undecided and the run is never declared env-funded. Dropping
// the late bit would let `OnlyEnvFunded()` skip the sovereign refusal on a
// value the runner resolves differently (ADR-129 § Objections, O1).
// Mutant: the late set dropped from the view → the field resolves to the
// gateway here → OnlyEnvFunded flips true → this test reds on the second
// assertion.
func TestRouteView_LateVarKeepsTheGateUndecided(t *testing.T) {
	t.Setenv("E2RV_X", "openai_compatible/gw-1")
	wf, _ := gatewayVarProgram("${E2RV_X:-openai_compatible/gw-1}")
	res := EffectiveProviders(RouteViewForLaunch[string](wf, ModelOverrides{}, nil, nil, nil), knownForTest)
	if res.NarrowSafe {
		t.Errorf("NarrowSafe = true, want false — a late var is as opaque as the raw ${…} it came from")
	}
	if res.OnlyEnvFunded() {
		t.Errorf("OnlyEnvFunded = true, want false — the sovereign refusal must stay armed on a late var")
	}
}

// The declared default whose value the RUN itself decided narrows: a
// literal gateway default resolves to the same model at dispatch, so the
// walk names it env-funded — the exact shape a templated route needs to
// become readable at the gate (#2005's shape).
// Mutant: envDeferred fed the RAW model again → envSeen flips →
// OnlyEnvFunded false → this test reds on the second assertion.
func TestRouteView_LiteralVarNarrows(t *testing.T) {
	wf, _ := gatewayVarProgram("openai_compatible/gw-1")
	res := EffectiveProviders(RouteViewForLaunch[string](wf, ModelOverrides{}, nil, nil, nil), knownForTest)
	if !res.NarrowSafe {
		t.Errorf("NarrowSafe = false, want true — the declared default is the run's own value")
	}
	if !res.OnlyEnvFunded() {
		t.Errorf("OnlyEnvFunded = false, want true — every route rides the gateway the runner's env funds")
	}
}

// A hint resolved through the run's vars is a NAME: recorded as the
// provider it will spend, never as an unknown template in the
// operator-facing diagnostic — and the attribution says WHERE the provider
// was pinned, site keys sorted, a site named twice by one chain held once.
// Mutant: resolveNode reads the RAW provider field again → Unknown holds
// `{{vars.p}}` → this test reds on the first assertion.
func TestProviderResolution_UnknownNeverHoldsATemplate(t *testing.T) {
	wf := &ir.Workflow{
		Vars: map[string]*ir.Var{
			"p": {Name: "p", Type: ir.VarString, HasDefault: true, Default: "openai"},
		},
		Nodes: map[string]ir.Node{
			// The chain names openai twice — one site, held once in the
			// pins — and a rescue names it again under its own key.
			"impl": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "impl"},
				LLMFields: ir.LLMFields{Backend: "claw", Provider: "{{vars.p}},{{vars.p}}", Model: "gpt-6"},
				Fallbacks: []ir.Fallback{{Name: "rescue", Provider: "openai", Model: "openai/gpt-6"}},
			},
		},
	}
	res := EffectiveProviders(RouteViewForLaunch[string](wf, ModelOverrides{}, nil, nil, nil), knownForTest)
	if len(res.Unknown) != 0 {
		t.Errorf("Unknown = %v, want empty — a resolved hint is a name, not a template", res.Unknown)
	}
	if want := []string{"openai"}; !slices.Equal(res.Providers, want) {
		t.Errorf("Providers = %v, want %v", res.Providers, want)
	}
	if want := []string{"impl", "impl#rescue"}; !slices.Equal(res.Pins["openai"], want) {
		t.Errorf("Pins[openai] = %v, want %v — the node and the route that named it, sorted, the duplicate collapsed", res.Pins["openai"], want)
	}
}

// A backend resolved through the run's vars is readable at the pre-flight:
// a route whose every field the vars decide either reads concretely or
// stays off the wire — never the undecided both-at-once a var-less reading
// gave (wire_reach.go).
// Mutant: the wire walk reads a var-less view again → the templated backend
// route returns and the claude_code model goes unreadable → this test reds
// on both assertions.
func TestWireRoutes_VarsResolvedBackendIsReadable(t *testing.T) {
	wf := &ir.Workflow{
		Vars: map[string]*ir.Var{
			"b": {Name: "b", Type: ir.VarString, HasDefault: true, Default: "claw"},
			"m": {Name: "m", Type: ir.VarString, HasDefault: true, Default: "claude-opus-5-5"},
		},
		Nodes: map[string]ir.Node{
			// gated: the vars resolved its backend onto claw, off the wire.
			"gated": clawAgent("gated", "openai", "openai/gpt-6"),
			// wireman rides the wire through a templated model on claude_code.
			"wireman": &ir.AgentNode{
				BaseNode:  ir.BaseNode{ID: "wireman"},
				LLMFields: ir.LLMFields{Backend: "claude_code", Provider: "anthropic", Model: "{{vars.m}}"},
			},
		},
	}
	wf.Nodes["gated"].(*ir.AgentNode).Backend = "{{vars.b}}"
	routes := AnthropicWireRoutes(RouteViewForLaunch[string](wf, ModelOverrides{}, nil, nil, nil))
	for _, r := range routes {
		if r.NodeID == "gated" {
			t.Errorf("gated route returned (%+v), want absent — the vars resolved it onto claw/openai", r)
		}
		if r.NodeID == "wireman" && !r.Readable {
			t.Errorf("wireman route unreadable (%+v), want the vars-resolved model read", r)
		}
	}
	if n := len(routes); n != 1 {
		t.Fatalf("routes = %d (%+v), want exactly the wireman one", n, routes)
	}
}

// The dispatch view reads msg.Vars through the SAME declared-only filter
// the executor is seeded with: an undeclared input is dropped, so a field
// templated on it stays undecided at the pre-flight exactly as it is
// undecided at dispatch — never narrowed on a value the run never has
// (every unattended lane sets AllowUnknownInputs).
// Mutant: the view reads msg.Vars raw → the undeclared value enters → the
// gate narrows → this test reds on the second assertion.
func TestDispatchView_DropsUndeclaredVars(t *testing.T) {
	wf, vars := gatewayVarProgram("openai_compatible/gw-1")
	view := RouteViewForDispatch(wf, ModelOverrides{}, nil, map[string]any{"m": "openai_compatible/gw-1", "b": "claw"})
	if _, present := view.Vars["b"]; present {
		t.Errorf("view.Vars carries b, want dropped — %v names no declared var", vars)
	}
	if got := view.Vars["m"]; got != "openai_compatible/gw-1" {
		t.Errorf("view.Vars[m] = %v, want the declared launch value kept", got)
	}
	res := EffectiveProviders(view, knownForTest)
	if !res.OnlyEnvFunded() {
		t.Errorf("OnlyEnvFunded = false, want true — the declared value decides the gateway route")
	}
}

// DecidedVars is the map a caller beside the fold resolves a routing value
// against: the reading's vars minus every late name — the same map the
// `when:` gate reads. A late name's value is the reading process's own env
// answer (an `${…}`-consulting default, a coercion failure kept raw), so a
// pin resolved over it would park, or spare, the run on this process's env;
// the field reading (field) keeps such a span as written.
// Mutant: DecidedVars returning Vars whole → the late name enters → this
// test reds on the first assertion.
func TestRouteView_DecidedVarsDropsEveryLateName(t *testing.T) {
	wf, _ := gatewayVarProgram("${ITERION_PROBE_M:-openai_compatible/gw-1}")
	view := RouteViewForDispatch[any](wf, ModelOverrides{}, nil, nil)
	if !view.LateVars["m"] {
		t.Fatal("an env-consulting default is not late — the witness does not exercise the drop")
	}
	decided := view.DecidedVars()
	if _, present := decided["m"]; present {
		t.Errorf("DecidedVars carries m (%v), want dropped — its value is this process's env answer", decided["m"])
	}
	if _, present := view.Vars["m"]; !present {
		t.Error("view.Vars lost m — the drop is the decided map's, the reading keeps the name")
	}
	// The gate reads the same map: one decided partition, two named readers.
	if !maps.Equal(decided, view.gateVars()) {
		t.Error("DecidedVars differs from what the when: gate reads — the two readers disagree on which names are decided")
	}
	// Nothing late: the map IS the view's, unchanged.
	literalWf, _ := gatewayVarProgram("openai_compatible/gw-1")
	plain := RouteViewForDispatch[any](literalWf, ModelOverrides{}, nil, nil)
	if !maps.Equal(plain.DecidedVars(), plain.Vars) {
		t.Error("with nothing late, DecidedVars differs from the view's vars")
	}
}
