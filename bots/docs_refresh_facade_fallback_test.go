package bots

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ast"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser/parsertest"
)

// TestDocsRefreshModelNodesFallBackToWhatTheirFacadeServes: every
// docs-refresh node that pins a claude id must declare a route for a model
// its credential cannot reach.
//
// The live failure (candidature, 2026-11-17) was typed and terminal: the
// z.ai facade refuses a claude id rather than answer it with GLM under the
// requested label, so `campaign` died with "the zai facade does not serve
// it" and the whole campaign was lost. The fallback is the remedy the
// refusal itself names, and it must point at an id the facade it names
// really serves — asserted through the production predicate, not a copy of
// its rule.
//
// Mutation (main.bot): drop the `fallbacks:` block from `campaign` (the
// live route), point the model at a claude id under provider zai (the
// adjudicating predicate refuses it), or drop `unavailable` from `on:`
// (the live category no longer routes) — each turns this test red.
func TestDocsRefreshModelNodesFallBackToWhatTheirFacadeServes(t *testing.T) {
	raw, err := botUnitSource("docs-refresh/main.bot")
	if err != nil {
		t.Fatalf("read docs-refresh: %v", err)
	}
	pr := parsertest.Parse("docs-refresh/main.bot", string(raw))
	if pr.File == nil {
		t.Fatal("docs-refresh/main.bot does not parse")
	}
	agents := map[string]*ast.AgentDecl{}
	for _, a := range pr.File.Agents {
		agents[a.Name] = a
	}
	// The nodes that pin a claude id: the campaign, the merge step every
	// converged pass takes, and the validator that decides convergence.
	for _, name := range []string{"campaign", "finalize_mr", "coverage_audit"} {
		a, ok := agents[name]
		if !ok {
			t.Fatalf("docs-refresh has no %q agent", name)
		}
		if len(a.Fallbacks) != 1 {
			t.Fatalf("%q declares %d fallback routes, want exactly the GLM rescue", name, len(a.Fallbacks))
		}
		fb := a.Fallbacks[0]
		if fb.Action != "" {
			t.Errorf("%q falls back with action %q: a terminal degrade would skip the pass, not run it elsewhere", name, fb.Action)
		}
		if fb.Backend != "" {
			t.Errorf("%q fallback pins backend %q: the node's claude_code must be inherited (a bare GLM id is not a claw spec)", name, fb.Backend)
		}
		if fb.Provider != "zai" {
			t.Errorf("%q fallback names provider %q, want zai so the route resolves its own key and base URL", name, fb.Provider)
		}
		if !fb.Metered {
			t.Errorf("%q fallback must carry `metered: true` — this route spends a metered credential", name)
		}
		on := strings.Join(fb.On, ",")
		for _, want := range []string{"unavailable", "auth", "usage_window"} {
			if !strings.Contains(on, want) {
				t.Errorf("%q fallback triggers on %q, which cannot take the %s class", name, on, want)
			}
		}
		// The route must serve what the facade it names actually answers:
		// the dial's default, expanded the way the engine expands it.
		model := ir.ExpandWithDefault(fb.Model, func(string) string { return "" })
		if model == "" || model == fb.Model {
			t.Errorf("%q fallback model %q does not expand to a default", name, fb.Model)
		}
		if !delegate.FacadeServesModelID(fb.Provider, model) {
			t.Errorf("%q falls back to %q, which the %s facade does not serve — the refusal this route cures", name, model, fb.Provider)
		}
	}
}
