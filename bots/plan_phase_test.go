package bots

import (
	"os"
	"path/filepath"
	"slices"
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
	"feature-gap-fill",
	"test-coverage",
	"e2e-coverage",
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

			// The peer policy defaults to "skip" fleet-wide: the
			// cross-model plan reviewer is an OPTIONAL enrichment, and
			// the primary family alone must always suffice — a dead
			// second-family credential parked a fixer holding a PR
			// hostage, and a stale pod key paused every cloud campaign
			// through plan_review auto + wait. "wait" stays the per-run
			// deliberate-spend opt-in.
			for varName, wantDefault := range map[string]string{
				"plan_review":        "auto",
				"plan_review_policy": "skip",
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
			// on: [any] is load-bearing: under the shipped sandbox:auto a
			// claw failure flattens to a string at the IPC boundary and
			// classifies UNCLASSIFIED, which a FILTERED skip refuses by
			// design — the operator's `skip` policy would silently become
			// `wait` (Revi finding R1b58ea).
			if len(skip.On) != 1 || skip.On[0] != "any" {
				t.Errorf("%s: skip route's on: = %v, want [any] (a filtered skip is refused unclassified failures — sandboxed claw errors classify unclassified)", bot, skip.On)
			}
			// And the policy var must be enum'd so a typo'd --var fails at
			// launch instead of silently selecting wait (Rde458c).
			if pol := wf.Vars["plan_review_policy"]; pol == nil || len(pol.EnumValues) != 2 {
				t.Errorf("%s: plan_review_policy lacks its [enum: wait, skip]", bot)
			}
		})
	}
}

// TestPlanPhaseCampaignEdgeMappings pins two authoring invariants the
// engine still relies on after #484. Exclusive FORWARD siblings are
// filtered to the selected edge; a loop-head RE-ENTRY still overlays
// the selected back-edge on every forward edge whose source has output
// (the back-edge is partial — it restates only the keys that change).
// So:
//
//  1. every FORWARD edge into `campaign` maps every campaign_input field
//     — an unmapped field is not "" but the raw {{input.x}} placeholder
//     leaking into the prompt;
//  2. no two LOOP back-edges into `campaign` map the same key — a shared
//     key lets the later-declared loop hijack it on every subsequent
//     pass (app-dev's draft_feedback/fail_log split exists because of
//     exactly this).
func TestPlanPhaseCampaignEdgeMappings(t *testing.T) {
	for _, bot := range planPhaseBots {
		t.Run(bot, func(t *testing.T) {
			path := filepath.Join(bot, "main.bot")
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			cr := ir.Compile(parser.Parse(path, string(src)).File)
			if cr.HasErrors() {
				t.Fatalf("%s does not compile: %+v", path, cr.Diagnostics)
			}
			wf := cr.Workflow

			schema, ok := wf.Schemas["campaign_input"]
			if !ok {
				t.Fatal("campaign_input schema missing")
			}
			var fields []string
			for _, f := range schema.Fields {
				fields = append(fields, f.Name)
			}

			loopKeyOwners := map[string]string{} // key -> "from(loop)"
			for _, e := range wf.Edges {
				if e.To != "campaign" {
					continue
				}
				mapped := map[string]bool{}
				for _, m := range e.With {
					mapped[m.Key] = true
				}
				if e.IsBoundedIteration() {
					for k := range mapped {
						owner := e.From + "(" + e.LoopName + e.ForeachName + ")"
						if prev, dup := loopKeyOwners[k]; dup {
							t.Errorf("%s: loop back-edges %s and %s both map %q — the later-declared one hijacks it on every pass",
								bot, prev, owner, k)
						}
						loopKeyOwners[k] = owner
					}
					continue
				}
				for _, f := range fields {
					if !mapped[f] {
						t.Errorf("%s: forward edge %s -> campaign does not map %q — the prompt renders a raw {{input.%s}} placeholder on that path",
							bot, e.From, f, f)
					}
				}
			}
			if len(loopKeyOwners) == 0 {
				t.Errorf("%s: no loop back-edge into campaign found — the continuation loop is gone?", bot)
			}

			// Declaration-order invariant: an UNTRAVERSED forward edge still
			// contributes its mappings (every edge whose source has output
			// applies, later declaration winning), so every forward edge into
			// campaign that maps a plan field to a LITERAL BLANK (the
			// phase-off / no-prior-content fallback: plan_topology's and
			// plan_gate's) must be declared BEFORE every forward edge that
			// maps it to REAL content, or the blank's untraversed
			// contribution clobbers the real value on pass 1.
			//
			// Expressed on the VALUE (blank vs. non-blank), not on a
			// hardcoded source node id: most bots hand the revised plan to
			// campaign directly from plan_revise, but a bot may route it
			// through an extra deterministic hop first (branch-improve-loop's
			// plan_budget_gate, native:695) — the invariant is the same
			// either way, and pinning the literal source name here would
			// make this shared test bot-specific.
			planFields := []string{"plan", "plan_critique", "plan_responses", "plan_provenance"}
			type fieldMapping struct {
				idx   int
				from  string
				blank bool
			}
			byField := map[string][]fieldMapping{}
			for i, e := range wf.Edges {
				if e.To != "campaign" || e.IsBoundedIteration() {
					continue
				}
				for _, m := range e.With {
					if !slices.Contains(planFields, m.Key) {
						continue
					}
					blank := len(m.Refs) == 0 && m.Raw == ""
					byField[m.Key] = append(byField[m.Key], fieldMapping{idx: i, from: e.From, blank: blank})
				}
			}
			for _, f := range planFields {
				mappings := byField[f]
				firstReal := -1
				for _, fm := range mappings {
					if !fm.blank && (firstReal == -1 || fm.idx < firstReal) {
						firstReal = fm.idx
					}
				}
				if firstReal == -1 {
					t.Errorf("%s: no forward edge into campaign maps %q to real content — the plan phase's output never reaches campaign", bot, f)
					continue
				}
				for _, fm := range mappings {
					if fm.blank && fm.idx > firstReal {
						t.Errorf("%s: %s -> campaign (idx %d) maps %q BLANK but is declared AFTER the real mapping (idx %d) — its untraversed contribution would clobber the real value", bot, fm.from, fm.idx, f, firstReal)
					}
				}
			}
		})
	}
}
