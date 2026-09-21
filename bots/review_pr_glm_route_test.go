package bots

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestReviewPrClaudeSlotFallsBackToGlmOnASpentForfait pins the claude slot's
// GLM route. Since 0.9.6 the primary itself runs on z.ai (all four reviewer
// nodes are pinned to provider "zai" + glm-5.3), so the route is the
// same-provider rescue at the older id — armed for a spent window, a rejected
// credential, or a model the facade refuses.
//
// review-pr is the gate every PR in this repo crosses, and it once declared no
// fallback of any kind: when the claude slot could not run, the reviewer
// simply failed.
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
//     provider from the model-spec prefix, so the element re-uses the
//     deployment's Anthropic credential — under the pin a different,
//     also-capped credential — or dies as `invalid spec` on a bare GLM id.
//   - the route names its own model — the older GLM id — so a hard failure of
//     the pinned id never re-issues an identical call with a second full
//     retry budget.
//   - `on:` names `auth`, which the DEFAULT trigger set excludes. It is the
//     category a present-but-rejected credential produces — under the pin,
//     the z.ai credential the primary itself runs on — and without `auth` the
//     route sleeps through it. (A z.ai key absent entirely fails fast at
//     authentication: the ambient Anthropic channels are stripped, so the CLI
//     reports "Not logged in" rather than serving the call from another
//     provider.) `unclassified` failures fall
//     through whatever the filter says (elementAccepts), which is what hides
//     the omission from a casual test.
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
		// Where the dial LANDS with nothing set, not the string it is
		// written as: `${ITERION_GLM_THING:-claude-opus-5}` carries "glm"
		// in the variable NAME and resolves to a Claude id. Expanded
		// against an empty lookup, so neither a developer's env nor the
		// ADR-093 bot_vars overlay can decide this test.
		landsOn := ir.ExpandWithDefault(route.Model, func(string) string { return "" })
		if !strings.Contains(strings.ToLower(landsOn), "glm") {
			t.Errorf("review-pr: node %q zai route model %q lands on %q — it must name a GLM id of "+
				"its own; inheriting (or repointing at) a Claude id sends the z.ai facade a model "+
				"it does not serve, and repointing at the node's own model re-uses the credential "+
				"that just failed", nodeID, route.Model, landsOn)
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
