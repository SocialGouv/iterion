package bots

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestReviewPrClaudeSlotFallsBackToGlmOnASpentForfait pins the route that
// lets Revi review at all when this instance's Anthropic credential cannot
// serve — window spent, credential rejected, or model unreachable.
//
// review-pr is the gate every PR in this repo crosses, and it declared no
// fallback of any kind: when the claude slot could not run, the reviewer
// simply failed. The recorded workaround was to point the GPT slot's model at
// GLM (docs/bot-runs/whole-improve-loop.md) — buying capacity with the
// cross-family check, on the one bot whose product IS that check.
//
// Four properties, each of which has a way of being lost silently:
//
//   - BOTH nodes of the claude slot carry the route. topology picks exactly
//     one of full-strength / glance per run, so a route on one of them covers
//     half the runs and looks correct in every test that only reads the other.
//   - the route carries NO `backend:`. This is the one that already went
//     wrong once. `provider:` is a hint, and providerFallbackEligible admits
//     exactly one backend — claude_code. Pin the element to claw (the
//     "obvious" home of GLM) and the hint is read by nobody: claw derives its
//     provider from the model-spec prefix, so `anthropic/…` resolves back to
//     the Anthropic credential that just failed, and the rescue re-uses it.
//   - the route names its own model. The node's model is a Claude id; the
//     element must carry the z.ai one or the facade is handed a model it
//     does not serve.
//   - `on:` names `auth`, which the DEFAULT trigger set excludes. It is the
//     category a present-but-rejected Anthropic credential produces: such a
//     credential still outranks z.ai in the CLI's precedence, so the CLI uses
//     it and 401s, and without `auth` the route sleeps through it. (An
//     instance with only a z.ai key never reaches this route at all —
//     claude_code resolves z.ai itself.) `unclassified` failures fall through
//     whatever the filter says (elementAccepts), which is what hides the
//     omission from a casual test.
func TestReviewPrClaudeSlotFallsBackToGlmOnASpentForfait(t *testing.T) {
	path := filepath.Join("review-pr", "main.bot")
	cr := ir.Compile(parseBotUnit(path).File)
	if cr.HasErrors() {
		t.Fatalf("%s does not compile: %+v", path, cr.Diagnostics)
	}

	for _, nodeID := range []string{"reviewer_claude", "reviewer_claude_glance"} {
		node, ok := cr.Workflow.Nodes[nodeID].(*ir.JudgeNode)
		if !ok {
			t.Fatalf("review-pr: node %q is %T, want *ir.JudgeNode", nodeID, cr.Workflow.Nodes[nodeID])
		}
		var route *ir.Fallback
		for i := range node.Fallbacks {
			if node.Fallbacks[i].Provider == "zai" {
				route = &node.Fallbacks[i]
			}
		}
		if route == nil {
			t.Errorf("review-pr: node %q declares no provider:zai route — when this instance's "+
				"Anthropic credential cannot serve, the claude slot has nowhere to go and the "+
				"review does not happen (fallbacks: %+v)", nodeID, node.Fallbacks)
			continue
		}
		if route.Backend != "" {
			t.Errorf("review-pr: node %q zai route pins backend %q — a hint is honoured on "+
				"claude_code and nowhere else, so pinning the element silently drops it and the "+
				"rescue resolves the credential that just failed", nodeID, route.Backend)
		}
		if !strings.Contains(strings.ToLower(route.Model), "glm") {
			t.Errorf("review-pr: node %q zai route model = %q — it must name a GLM id of its own; "+
				"inheriting (or repointing at) a Claude id sends the z.ai facade a model it does "+
				"not serve, and repointing at the node's own model re-uses the credential that "+
				"just failed", nodeID, route.Model)
		}
		if route.When != "" {
			t.Errorf("review-pr: node %q zai route is gated on when: %q — a `when:` is evaluated at "+
				"dispatch, so a false one leaves the route in the file and unreachable at runtime",
				nodeID, route.When)
		}
		if !route.Metered {
			t.Errorf("review-pr: node %q zai route dropped metered: — the route spends a billed key "+
				"where the primary spends a subscription, and authoring it IS the consent (ADR-087)",
				nodeID)
		}
		// The WHOLE set, not just its interesting member: an assertion on
		// `auth` alone lets the production trigger (usage_window) be
		// deleted in silence.
		if want := []string{"usage_window", "auth", "unavailable"}; !slices.Equal(route.On, want) {
			t.Errorf("review-pr: node %q zai route on: = %v, want %v. `auth` is the one that reads "+
				"like a stray: a present but rejected Anthropic credential still outranks z.ai, so "+
				"the CLI uses it and 401s, and auth is NOT in the default trigger set — without it "+
				"the route sleeps through exactly that instance. `usage_window` is the production "+
				"case and `unavailable` covers a model the resolved credential cannot reach",
				nodeID, route.On, want)
		}
	}
}
