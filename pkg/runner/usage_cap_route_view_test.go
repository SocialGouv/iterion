package runner

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// The usage-cap pre-flight reads its wire routes through the run's vars
// (model.RouteViewForDispatch): a routing field the launch's declared
// defaults decide is read by that answer — a run whose every route the
// vars resolve onto claw/openai cannot spend the capped subscription and
// is not parked for it, where the var-less reading of #668 parked it for
// a route it could not take.
// Mutant: the pre-flight reads a var-less view again (the view argument
// reverted) → the templated backend stays undecided → the route counts as
// wire-reachable → the run parks → this test reds on the first assertion.
func TestUsageCapPreflight_TemplatedBackendOffWire(t *testing.T) {
	blankAnthropicWireEnv(t)
	hard := usagecap.NewMemStore()
	weekCapped(t, hard, platformKey("fp-forfait"), time.Now().UTC().Add(30*time.Hour))
	ctx := forfaitBesidePinnedKeys()

	templated := func(def string) *ir.Workflow {
		wf := chainWorkflow(agentRoute("impl", "{{vars.b}}", "openai", "openai/gpt-6"))
		wf.Vars = map[string]*ir.Var{"b": varString("b", def)}
		return wf
	}
	// The declared default decides claw: every route reads off the wire.
	if err := preflightFor(t, hard, ctx, templated("claw")); err != nil {
		t.Errorf("parked a run whose every route the launch vars resolve off the wire: %v", err)
	}
	// Control: the same template over a default on the wire — the cap holds.
	if err := preflightFor(t, hard, ctx, templated("claude_code")); err == nil {
		t.Error("started a run whose templated backend resolves onto the capped wire")
	}
}
