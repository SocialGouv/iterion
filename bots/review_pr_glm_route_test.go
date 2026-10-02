package bots

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// TestReviewPrClaudeSlotFallsBackToGlmOnASpentForfait pins the claude slot's
// GLM route. The primary runs on the instance's Anthropic credential, so the
// route is the rescue — armed for a spent usage window, a rejected
// credential, or a model the credential cannot reach.
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
//     deployment's Anthropic credential — the one that just failed — or dies
//     as `invalid spec` on a bare GLM id.
//   - the route names its own model — a GLM id of its own — so a hard failure
//     of the primary never re-issues an identical call with a second full
//     retry budget.
//   - `on:` names `auth`, which the DEFAULT trigger set excludes. It is the
//     category a present-but-rejected Anthropic credential produces under
//     the pinned-Anthropic primary — the CLI uses it and 401s — and it is
//     where an instance with ONLY a z.ai key lands too (the forced-Anthropic
//     primary has no Anthropic channel at all). Without `auth` the route
//     sleeps through exactly that instance. (With no z.ai key at all the
//     route is refused before the CLI spawns — ErrNoFacadeCredential names
//     zai and ZAI_API_KEY — rather than served by another provider.)
//     `unclassified` failures fall
//     through whatever the filter says (elementAccepts), which is what hides
//     the omission from a casual test.
func TestReviewPrClaudeSlotFallsBackToGlmOnASpentForfait(t *testing.T) {
	// converge rides EVERY path: with no second server a capped forfait
	// costs the whole review even when the reviewer was rescued, and unpinned
	// its claude id goes to the z.ai facade.
	assertAnthropicPinnedWithGlmRoute(t, "review-pr", "reviewer_claude", "reviewer_claude_glance", "converge")
}

// Revi's conversational sibling answers `/revi <question>` on the same claude
// slot and dials: the same facade hijack, the same missing lane.
func TestReviConverseAgentFallsBackToGlmOnASpentForfait(t *testing.T) {
	assertAnthropicPinnedWithGlmRoute(t, "revi-converse", "converse_agent")
}

func assertAnthropicPinnedWithGlmRoute(t *testing.T, bot string, nodeIDs ...string) {
	t.Helper()
	path := filepath.Join(bot, "main.bot")
	cr := ir.Compile(parseBotUnit(path).File)
	if cr.HasErrors() {
		t.Fatalf("%s does not compile: %+v", path, cr.Diagnostics)
	}
	for _, nodeID := range nodeIDs {
		node, ok := cr.Workflow.Nodes[nodeID].(ir.LLMNode)
		if !ok {
			t.Fatalf("%s: node %q is %T, want an LLM node", bot, nodeID, cr.Workflow.Nodes[nodeID])
		}
		// The PRIMARY is pinned Anthropic-direct. The wire's credential order
		// (secrets.AnthropicWireSlotOrder) puts facade keys (zai, moonshot)
		// FIRST, and a z.ai key becomes the run's DEFAULT whenever the
		// publisher skips a capped forfait: an unpinned claude node then
		// sends its claude id to the z.ai facade, which ACCEPTS it and serves
		// GLM in silence, under the claude label, with the route below never
		// firing. Expanded like the model dial: a ${VAR:-anthropic} dial
		// that lands elsewhere reddens.
		if lands := ir.ExpandWithDefault(node.GetLLMFields().Provider, func(string) string { return "" }); lands != "anthropic" {
			t.Errorf("%s: node %q provider %q lands on %q, want \"anthropic\" — without the pin the "+
				"wire's facade-first credential order (zai before the forfait) hands a claude model id to "+
				"a facade that answers it with GLM in silence, under the claude label",
				bot, nodeID, node.GetLLMFields().Provider, lands)
		}
		var route *ir.Fallback
		fallbacks := node.GetFallbacks()
		for i := range fallbacks {
			if fallbacks[i].Provider == "zai" {
				route = &fallbacks[i]
			}
		}
		if route == nil {
			t.Errorf("%s: node %q declares no provider:zai route — when this instance's "+
				"Anthropic credential cannot serve, the node has nowhere to go and the "+
				"work does not happen (fallbacks: %+v)", bot, nodeID, fallbacks)
			continue
		}
		if route.Backend != "" {
			t.Errorf("%s: node %q zai route pins backend %q — a hint is honoured on "+
				"claude_code and nowhere else, so pinning the element silently drops it and the "+
				"rescue resolves the credential that just failed", bot, nodeID, route.Backend)
		}
		// Where the dial LANDS with nothing set, not the string it is
		// written as: `${ITERION_GLM_THING:-claude-opus-5}` carries "glm"
		// in the variable NAME and resolves to a Claude id. Expanded
		// against an empty lookup, so neither a developer's env nor the
		// ADR-093 bot_vars overlay can decide this test. GLM is always 5.3.
		landsOn := ir.ExpandWithDefault(route.Model, func(string) string { return "" })
		if landsOn != "glm-5.3" {
			t.Errorf("%s: node %q zai route model %q lands on %q, want glm-5.3 — it must name a GLM id "+
				"of its own (inheriting a Claude id sends the z.ai facade a model it maps on its own "+
				"terms, and the node's own model re-uses the credential that just failed), and GLM "+
				"is always 5.3", bot, nodeID, route.Model, landsOn)
		}
		if route.When != "" {
			t.Errorf("%s: node %q zai route is gated on when: %q — a `when:` is evaluated at "+
				"dispatch, so a false one leaves the route in the file and unreachable at runtime",
				bot, nodeID, route.When)
		}
		if !route.Metered {
			t.Errorf("%s: node %q zai route dropped metered: — the route spends a billed key "+
				"where the primary spends a subscription, and authoring it IS the consent (ADR-087)",
				bot, nodeID)
		}
		// The WHOLE set, not just its interesting member: an assertion on
		// `auth` alone lets the production trigger (usage_window) be
		// deleted in silence.
		if want := []string{"usage_window", "auth", "unavailable"}; !slices.Equal(route.On, want) {
			t.Errorf("%s: node %q zai route on: = %v, want %v. `auth` is the one that reads "+
				"like a stray: a present but rejected Anthropic credential still outranks z.ai, so "+
				"the CLI uses it and 401s, and auth is NOT in the default trigger set — without it "+
				"the route sleeps through exactly that instance. `usage_window` is the production "+
				"case and `unavailable` covers a model the resolved credential cannot reach",
				bot, nodeID, route.On, want)
		}
	}
}
