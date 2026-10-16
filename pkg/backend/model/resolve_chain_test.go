package model

import (
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func chainAgentNode(id, backend, provider string, fbs []ir.Fallback) *ir.AgentNode {
	n := &ir.AgentNode{}
	n.ID = id
	n.Backend = backend
	n.Provider = provider
	n.Fallbacks = fbs
	return n
}

// TestResolveChain_AppendsFallbacksAfterProviderChain pins how the two
// surfaces compose: the legacy `provider:` hints are walked first (they
// swap a credential on one backend), then each `fallbacks:` route in
// declaration order.
func TestResolveChain_AppendsFallbacksAfterProviderChain(t *testing.T) {
	e := &ClawExecutor{}
	node := chainAgentNode("x", delegate.BackendClaudeCode, "zai,anthropic", []ir.Fallback{
		{Name: "api", Backend: delegate.BackendClaw, Model: "openai/gpt-5.5"},
	})
	chain := e.resolveChain(node)
	if len(chain) != 3 {
		t.Fatalf("chain length %d, want 3 (two hints + one route): %+v", len(chain), chain)
	}
	if chain[0].Provider != "zai" || chain[1].Provider != "anthropic" {
		t.Errorf("provider hints not first: %+v", chain)
	}
	if chain[2].Label != "api" || chain[2].Backend != delegate.BackendClaw {
		t.Errorf("fallback route not appended: %+v", chain[2])
	}
}

func TestResolveChain_PreservesRunFallbackStageIndex(t *testing.T) {
	e := &ClawExecutor{}
	node := chainAgentNode("x", delegate.BackendClaudeCode, "zai,anthropic", []ir.Fallback{
		{
			Name:        ir.RunFallbackName,
			Backend:     delegate.BackendClaw,
			Model:       "openai/gpt-5.5",
			RunStage:    1,
			RunStageSet: true,
		},
	})
	chain := e.resolveChain(node)
	stage := chain[len(chain)-1].FallbackIndex
	if stage == nil || *stage != 1 {
		t.Fatalf("run fallback index = %v, want original launch stage 1", stage)
	}
}

// TestResolveChain_DefaultTriggers: a route that declares no `on:` gets
// the closed positive default. `any` is excluded because a budget cap or
// a schema-shape failure re-fails identically on every route; `auth` is
// excluded because AuthFailedRecipe deliberately routes a dead
// credential to a human.
func TestResolveChain_DefaultTriggers(t *testing.T) {
	e := &ClawExecutor{}
	node := chainAgentNode("x", delegate.BackendClaudeCode, "", []ir.Fallback{
		{Name: "api", Backend: delegate.BackendClaw, Model: "openai/gpt-5.5"},
	})
	on := e.resolveChain(node)[1].On
	if len(on) != 2 {
		t.Fatalf("default triggers = %v, want two", on)
	}
	want := map[delegate.FallbackCategory]bool{
		delegate.FallbackUsageWindow: true,
		delegate.FallbackUnavailable: true,
	}
	for _, c := range on {
		if !want[c] {
			t.Errorf("unexpected default trigger %q", c)
		}
	}
	if elementAccepts(chainElement{On: on}, delegate.FallbackAuth) {
		t.Error("the default trigger set must not route on auth — that reverses AuthFailedRecipe's deliberate pause-for-human")
	}
}

// TestResolveChain_AnyClearsTheFilter: an explicit `any` means accept
// every condition, which the walker represents as no filter at all.
func TestResolveChain_AnyClearsTheFilter(t *testing.T) {
	e := &ClawExecutor{}
	node := chainAgentNode("x", delegate.BackendClaudeCode, "", []ir.Fallback{
		{Name: "api", Backend: delegate.BackendClaw, Model: "openai/gpt-5.5", On: []string{"any"}},
	})
	el := e.resolveChain(node)[1]
	if el.On != nil {
		t.Fatalf("on: [any] should clear the filter, got %v", el.On)
	}
	if !elementAccepts(el, delegate.FallbackAuth) {
		t.Error("on: [any] must accept every condition")
	}
}

// TestChainIsHintOnly_NamedRouteIsNeverCollapsed: the collapse guard
// exists to stop a hint-ignoring backend re-running an identical call.
// A named route is a distinct call the author declared — even one that
// only varies the model, which IS meaningful on claw (it derives its
// provider from the model-spec prefix).
func TestChainIsHintOnly_NamedRouteIsNeverCollapsed(t *testing.T) {
	legacy := []chainElement{{Provider: "zai"}, {Provider: "anthropic"}}
	if !chainIsHintOnly(legacy) {
		t.Error("a bare provider chain is hint-only")
	}
	if got := collapseHintOnlyChain(legacy, delegate.BackendClaw); len(got) != 1 {
		t.Errorf("a hint-only chain on claw must collapse, got %d elements", len(got))
	}

	authored := []chainElement{{}, {Label: "api", Model: "openai/gpt-5.5"}}
	if chainIsHintOnly(authored) {
		t.Error("a chain containing a named route is not hint-only")
	}
	if got := collapseHintOnlyChain(authored, delegate.BackendClaw); len(got) != 2 {
		t.Errorf("an authored chain must survive on claw, got %d elements", len(got))
	}
}

// TestResolveChain_EnvExpansion: every field of a route may carry a
// ${VAR} ref, resolved at run time like the node's own backend/model.
func TestResolveChain_EnvExpansion(t *testing.T) {
	e := &ClawExecutor{}
	node := chainAgentNode("x", delegate.BackendClaudeCode, "", []ir.Fallback{
		{Name: "api", Backend: "${FB_BACKEND:-claw}", Model: "${FB_MODEL:-openai/gpt-5.5}"},
	})
	el := e.resolveChain(node)[1]
	if el.Backend != delegate.BackendClaw || el.Model != "openai/gpt-5.5" {
		t.Errorf("env refs not expanded: backend=%q model=%q", el.Backend, el.Model)
	}
}

// TestResolveChain_NoFallbacksIsUnchanged guards the blast radius: a
// node with no block resolves to exactly what it resolved to before.
func TestResolveChain_NoFallbacksIsUnchanged(t *testing.T) {
	e := &ClawExecutor{}
	node := chainAgentNode("x", delegate.BackendClaudeCode, "zai,anthropic", nil)
	if got, want := len(e.resolveChain(node)), len(e.resolveProviderChain(node)); got != want {
		t.Errorf("chain length %d, want %d — a node with no fallbacks: must be untouched", got, want)
	}
}

// TestDedupeChain_DropsIdenticalRoute keeps the protection the old
// providerFallbackEligible collapse bought: a route resolving to the
// same backend+credential+model as its predecessor cannot succeed where
// that one failed, and would pay a second full retry budget to prove it.
// Comparison is on the EFFECTIVE backend, so a route naming the node's
// own backend explicitly is recognised as the duplicate it is.
func TestDedupeChain_DropsIdenticalRoute(t *testing.T) {
	e := &ClawExecutor{}
	node := chainAgentNode("x", delegate.BackendClaudeCode, "", []ir.Fallback{
		{Name: "same", Backend: delegate.BackendClaudeCode},
	})
	if got := e.resolveChain(node); len(got) != 1 {
		t.Errorf("a route identical to the node's own must be dropped, got %+v", got)
	}
}

// TestDedupeChain_DropsRestatedModel: a route that spells out the node's
// own model rather than inheriting it is the same call, and would
// otherwise pay a second full retry budget to fail identically.
func TestDedupeChain_DropsRestatedModel(t *testing.T) {
	e := &ClawExecutor{}
	node := chainAgentNode("x", delegate.BackendClaudeCode, "", []ir.Fallback{
		{Name: "same", Backend: delegate.BackendClaudeCode, Model: "claude-opus-5"},
	})
	node.Model = "claude-opus-5"
	if got := e.resolveChain(node); len(got) != 1 {
		t.Errorf("a route restating the node's own model must be dropped, got %+v", got)
	}
}

// TestDedupeChain_KeepsDistinctRoutes guards the other direction: the
// dedup must not eat a route that genuinely differs.
func TestDedupeChain_KeepsDistinctRoutes(t *testing.T) {
	got := dedupeChain([]chainElement{
		{},
		{Label: "api", Backend: "claw", Model: "openai/gpt-5.5"},
		{Label: "gpt", Backend: "claw", Model: "openai/gpt-5.4-mini"},
	}, "claude_code", "claude-opus-5")
	if len(got) != 3 {
		t.Errorf("distinct routes were deduped: %+v", got)
	}
}

// TestEffectiveFallbackBackends_IsTheChainsBackendProjection pins the contract
// the engine's pre-run analyses read: the backends THIS run's chain may take,
// and nothing else. Every shape a route can have is in one node, because the
// shapes that must be absent are what makes the answer usable — a caller reads
// it as the whole set of routes that leave the node's own backend.
func TestEffectiveFallbackBackends_IsTheChainsBackendProjection(t *testing.T) {
	t.Setenv("ITERION_E2_UNSET_ROUTE", "")
	e := &ClawExecutor{staticProgramOnly: true}
	e.SetVars(map[string]any{"fb": delegate.BackendClaudeCode, "rescue": "off"})
	node := chainAgentNode("x", delegate.BackendClaw, "", []ir.Fallback{
		{Name: "templated", Backend: "{{vars.fb}}", Model: "anthropic/claude-haiku-4-5"},
		{Name: "gated", Backend: delegate.BackendCodex, Model: "openai/gpt-5.5", When: "vars.rescue == 'on'"},
		{Name: "inherits", Model: "anthropic/claude-haiku-4-5"},
		{Name: "unset-ref", Backend: "${ITERION_E2_UNSET_ROUTE}", Model: "anthropic/claude-haiku-4-5"},
		{Name: "same-as-templated", Backend: delegate.BackendClaudeCode, Model: "anthropic/claude-haiku-4-5"},
		{Name: "own-backend-other-credential", Backend: delegate.BackendClaw, Provider: "zai", Model: "anthropic/claude-haiku-4-5"},
	})
	got := e.EffectiveFallbackBackends(node)
	// The shapes that must be ABSENT are asserted FIRST, and each on its own:
	// read after a length check they are unreachable, and the length is the one
	// assertion whose message cannot say which shape leaked.
	for _, absent := range []string{delegate.BackendClaw, "", "{{vars.fb}}", "${ITERION_E2_UNSET_ROUTE}"} {
		for _, b := range got {
			if b == absent {
				t.Errorf("the answer carries %q (%v): this node's routes leave claw for claude_code and nowhere else — the node's own backend is EffectiveBackendName's answer, on its `provider:` variant too; a route that names none inherits it; a `${VAR}` that resolves to nothing inherits it; and a template this run resolves is named by what it resolved to", absent, got)
			}
		}
	}
	want := []string{delegate.BackendClaudeCode}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("EffectiveFallbackBackends = %v, want %v — the run resolves `{{vars.fb}}` to claude_code; the `when:` gate is off for these vars, the route that names no backend inherits the node's, a `${VAR}` that resolves to nothing inherits it too, a route onto the node's own backend with another credential is the node's own route, and a second route onto the same backend adds no route to read", got, want)
	}
	// The gate is read, not assumed: the same chain with the var on carries it.
	// Asserted as the WHOLE answer: the templated route's resolution is this
	// test's own subject, so it is named here rather than left to a length the
	// rest of the chain happens to pin — this seam legitimately returns a
	// `{{…}}` span for a var it cannot resolve.
	e.SetVars(map[string]any{"rescue": "on"})
	gateOn := []string{delegate.BackendClaudeCode, delegate.BackendCodex}
	if got := e.EffectiveFallbackBackends(node); !slices.Equal(got, gateOn) {
		t.Errorf("EffectiveFallbackBackends = %v with rescue=on, want %v — the gate is read against the vars this executor holds, and SetVars merges, so the templated route still resolves to claude_code", got, gateOn)
	}
}

// A route that executes nothing resolves no backend either. `action: skip`
// completes the node where it is reached, so the chain never asks the registry
// for a backend beside it — and a projection that named one would hand a
// pre-run analysis a route the chain cannot run on.
//
// The shape cannot come from a compiled .bot: the compiler refuses `action:
// skip` together with a backend (checkFallbackAction). It can come from an IR
// built in process, and the answer is the chain dispatch walks either way.
func TestEffectiveFallbackBackends_ASkipRouteResolvesNoBackend(t *testing.T) {
	e := &ClawExecutor{staticProgramOnly: true}
	node := chainAgentNode("x", delegate.BackendClaw, "", []ir.Fallback{
		{Name: "give-up", Action: ir.FallbackActionSkip, Backend: delegate.BackendClaudeCode},
	})
	// The drop rests on a fact about the chain dispatch walks, and it is
	// asserted rather than assumed: the element IS in that chain, carrying its
	// own backend, and what makes the drop sound is that dispatch resolves no
	// backend beside a skip element. A walker that stopped carrying it would
	// leave this test green on an answer that proves nothing.
	chain := e.resolveChain(node)
	if last := chain[len(chain)-1]; !last.Skip || last.Backend != delegate.BackendClaudeCode {
		t.Fatalf("precondition: the chain dispatch walks no longer carries the skip element with its own backend: %+v", chain)
	}
	if got := e.EffectiveFallbackBackends(node); len(got) != 0 {
		t.Errorf("EffectiveFallbackBackends = %v for a node whose only route is an `action: skip`: the chain completes the node there and resolves no backend, so a reader of this answer would judge the node on a route it can never run on", got)
	}
}

// A `when:` gate the executor cannot evaluate deactivates its route, and the
// projection says so because the CHAIN says so: the two read the same
// fallbackWhenActive, so a pre-run analysis that drops the route drops one
// dispatch will not walk either. The compiler refuses a gate that does not
// parse (C173), so the cell is reachable only from an IR built in process.
func TestEffectiveFallbackBackends_AGateTheExecutorCannotEvaluateDropsItsRoute(t *testing.T) {
	e := &ClawExecutor{staticProgramOnly: true}
	node := chainAgentNode("x", delegate.BackendClaw, "", []ir.Fallback{
		{Name: "alt", Backend: delegate.BackendClaudeCode, Model: "anthropic/claude-haiku-4-5", When: "vars.rescue ===== ("},
	})
	for _, el := range e.resolveChain(node) {
		if el.Backend == delegate.BackendClaudeCode {
			t.Fatalf("precondition: dispatch's own chain carries the route of a gate it cannot evaluate: %+v", el)
		}
	}
	if got := e.EffectiveFallbackBackends(node); len(got) != 0 {
		t.Errorf("EffectiveFallbackBackends = %v where the chain dispatch walks carries no such route: the projection and the chain must read one gate, not two", got)
	}
}

// The reading a caller may take per FIELD is the one dispatch takes per CHAIN.
// Asserted against resolveChain's own elements, so a caller resolving a route's
// `backend:` on its own — pkg/dryrun, which keeps the author's spelling of a
// templated field on an under-specified launch and resolves every other — can
// never name a route otherwise than the chain names it.
func TestTheExportedRouteReadingIsTheOneTheChainTakes(t *testing.T) {
	e := &ClawExecutor{staticProgramOnly: true}
	e.SetVars(map[string]any{"fb": delegate.BackendClaudeCode, "rescue": "off"})
	routes := []ir.Fallback{
		{Name: "templated", Backend: "{{vars.fb}}", Model: "anthropic/claude-haiku-4-5"},
		{Name: "gated-off", Backend: delegate.BackendCodex, Model: "openai/gpt-5.5", When: "vars.rescue == 'on'"},
		{Name: "gated-on", Backend: delegate.BackendPi, Model: "anthropic/claude-haiku-4-5", When: "vars.rescue == 'off'"},
	}
	node := chainAgentNode("x", delegate.BackendClaw, "", routes)
	carried := map[string]string{}
	for _, el := range e.resolveChain(node) {
		if el.Label != "" {
			carried[el.Label] = el.Backend
		}
	}
	compared := 0
	for _, fb := range routes {
		resolved, inChain := carried[fb.Name]
		if !inChain {
			continue
		}
		compared++
		if got := e.ResolveRoutingField(fb.Backend); got != resolved {
			t.Errorf("ResolveRoutingField(%q) = %q while the chain resolved that route to %q: one reading of a routing field, read once", fb.Backend, got, resolved)
		}
	}
	// The match is on the chain's own label, which this test does not own: with
	// no row compared every assertion above disappears, and the comparison
	// stops being made rather than failing.
	if compared == 0 {
		t.Fatalf("no route of the chain was compared: the chain labels its `fallbacks:` elements otherwise than by name, so this test asserts nothing (chain labels: %v)", carried)
	}
}
