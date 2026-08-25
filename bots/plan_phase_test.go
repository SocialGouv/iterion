package bots

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// planPhaseBots are the campaign bots carrying the cross-model plan
// phase (author → peer critique → author revise → campaign). The list is
// the contract: adding the phase to another bot means adding it here so
// the wiring below is guarded for it too.
var planPhaseBots = []string{
	"feature-dev",
	"app-dev",
	"branch-improve-loop",
	"whole-improve-loop",
}

// TestPlanPhaseWiring pins, per bot, the parts of the plan phase whose
// silent loss would not fail compilation:
//   - the plan_review / plan_review_policy vars (the launch resolver's
//     opt-in gate — without the declaration, reviewtopology injects
//     nothing and the phase is dead config);
//   - the phase nodes;
//   - the peer reviewer's `action: skip` fallback route gated on
//     plan_review_policy (the "continue and ignore" half of the policy).
func TestPlanPhaseWiring(t *testing.T) {
	for _, bot := range planPhaseBots {
		t.Run(bot, func(t *testing.T) {
			path := filepath.Join(bot, "main.bot")
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			pr := parser.Parse(path, string(src))
			cr := ir.Compile(pr.File)
			if cr.HasErrors() {
				t.Fatalf("%s does not compile: %+v", path, cr.Diagnostics)
			}
			wf := cr.Workflow

			for varName, wantDefault := range map[string]string{
				"plan_review":        "auto",
				"plan_review_policy": "wait",
			} {
				v, ok := wf.Vars[varName]
				if !ok {
					t.Errorf("%s: var %s not declared — the launch resolver injects nothing without it", bot, varName)
					continue
				}
				if got, _ := v.Default.(string); got != wantDefault {
					t.Errorf("%s: var %s default = %q, want %q", bot, varName, got, wantDefault)
				}
			}

			for _, nodeID := range []string{"plan_topology", "plan", "plan_review", "plan_gate", "plan_revise"} {
				if _, ok := wf.Nodes[nodeID]; !ok {
					t.Errorf("%s: plan-phase node %q missing", bot, nodeID)
				}
			}

			peer, ok := wf.Nodes["plan_review"].(*ir.JudgeNode)
			if !ok {
				t.Fatalf("%s: plan_review is not a judge node", bot)
			}
			var skip *ir.Fallback
			for i := range peer.Fallbacks {
				if peer.Fallbacks[i].Action == ir.FallbackActionSkip {
					skip = &peer.Fallbacks[i]
				}
			}
			if skip == nil {
				t.Fatalf("%s: plan_review has no action:skip fallback route — the skip policy is dead", bot)
			}
			if !strings.Contains(skip.When, "plan_review_policy") {
				t.Errorf("%s: skip route's when: %q is not gated on plan_review_policy", bot, skip.When)
			}
			wantOn := map[string]bool{"usage_window": true, "unavailable": true, "auth": true}
			for _, on := range skip.On {
				delete(wantOn, on)
			}
			if len(wantOn) > 0 {
				t.Errorf("%s: skip route's on: filter missing %v", bot, wantOn)
			}
		})
	}
}
