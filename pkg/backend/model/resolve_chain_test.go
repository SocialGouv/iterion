package model

import (
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

func chainJudgeNode(id, backend string, fbs []ir.Fallback) *ir.JudgeNode {
	n := &ir.JudgeNode{}
	n.ID = id
	n.Backend = backend
	n.Fallbacks = fbs
	return n
}

// TestRunFallback_AppliesToAgentWithNoRoutes is the operator surface's
// whole point: a bot that declares nothing still survives a forfait
// wall when the operator asks it to at launch.
func TestRunFallback_AppliesToAgentWithNoRoutes(t *testing.T) {
	e := &ClawExecutor{runFallback: RunFallback{
		Backend: delegate.BackendClaw,
		Model:   "openai/gpt-5.5",
	}}
	chain := e.resolveChain(chainAgentNode("x", delegate.BackendClaudeCode, "", nil))
	if len(chain) != 2 {
		t.Fatalf("chain length %d, want 2: %+v", len(chain), chain)
	}
	if chain[1].Label != runFallbackLabel || chain[1].Backend != delegate.BackendClaw {
		t.Errorf("run-level route not appended: %+v", chain[1])
	}
	// It inherits the same closed positive default as an authored route.
	if elementAccepts(chain[1], delegate.FallbackAuth) {
		t.Error("the run-level route must not route on auth by default")
	}
}

// TestRunFallback_NeverAppliesToJudges: a judge's verdict is
// load-bearing — a weaker model still emits a well-formed verdict and
// only the finding COUNT changes, which a deterministic merge gate
// reads. A blanket launch setting must never reach it.
func TestRunFallback_NeverAppliesToJudges(t *testing.T) {
	e := &ClawExecutor{runFallback: RunFallback{
		Backend: delegate.BackendClaw,
		Model:   "openai/gpt-5.5",
	}}
	chain := e.resolveChain(chainJudgeNode("gate", delegate.BackendClaudeCode, nil))
	for _, el := range chain {
		if el.Label == runFallbackLabel {
			t.Fatalf("the run-level route reached a judge: %+v", chain)
		}
	}
}

// TestRunFallback_AuthoredRoutesWin: an author who wrote a chain vetted
// where it may go. Appending the operator's route past their last one
// would extend a deliberate stop.
func TestRunFallback_AuthoredRoutesWin(t *testing.T) {
	e := &ClawExecutor{runFallback: RunFallback{
		Backend: delegate.BackendClaw,
		Model:   "openai/gpt-5.5",
	}}
	node := chainAgentNode("x", delegate.BackendClaudeCode, "", []ir.Fallback{
		{Name: "api", Backend: delegate.BackendClaw, Model: "anthropic/claude-opus-5"},
	})
	chain := e.resolveChain(node)
	for _, el := range chain {
		if el.Label == runFallbackLabel {
			t.Fatalf("the run-level route was appended past an authored chain: %+v", chain)
		}
	}
}

// TestDedupeChain_DropsIdenticalRoute keeps the protection the old
// providerFallbackEligible collapse bought: a route resolving to the
// same backend+credential+model as its predecessor cannot succeed where
// that one failed, and would pay a second full retry budget to prove it.
func TestDedupeChain_DropsIdenticalRoute(t *testing.T) {
	e := &ClawExecutor{runFallback: RunFallback{Backend: delegate.BackendClaudeCode}}
	// The operator's route names the backend the node already runs on
	// and pins no model — nothing about the call would change.
	chain := e.resolveChain(chainAgentNode("x", delegate.BackendClaudeCode, "", nil))
	if len(chain) != 1 {
		t.Errorf("a run-level route identical to the node's own must be dropped, got %+v", chain)
	}
}

// TestDedupeChain_KeepsDistinctRoutes guards the other direction: the
// dedup must not eat a route that genuinely differs.
func TestDedupeChain_KeepsDistinctRoutes(t *testing.T) {
	got := dedupeChain([]chainElement{
		{},
		{Label: "api", Backend: "claw", Model: "openai/gpt-5.5"},
		{Label: "gpt", Backend: "claw", Model: "openai/gpt-5.4-mini"},
	}, "claude_code")
	if len(got) != 3 {
		t.Errorf("distinct routes were deduped: %+v", got)
	}
}
