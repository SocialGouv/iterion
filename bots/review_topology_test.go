package bots

import (
	"os"
	"strings"
	"testing"
)

// The bi-model review-loop bots run their reviewer alternation through a
// `condition` router driven by review_mode/mono_family (pkg/reviewtopology),
// NOT a fixed `round_robin`. round_robin ignores `when` edge guards
// (pkg/runtime/routing.go), so reverting one of these routers to round_robin
// would silently defeat MONO — the second family would fire every pass again,
// and (for the family-clause bots) MONO would never converge. This test is
// the regression guard: it fails if a review-loop bot drops the topology vars
// or reintroduces a `mode: round_robin` router directive.
//
// When a genuinely new review-loop bot is added, add it here.
//
// whole-improve-loop and branch-improve-loop are intentionally NOT in this
// set: their v2 shape (ADR-058 — one adaptive `campaign` agent that
// self-reviews via a fresh re-review of the work + the deterministic
// build/test gate + its own judgment, the operator's proven manual pattern)
// has no cross-family reviewer nodes and no `condition` review router, so the
// ADR-052 mono/dual topology invariant does not apply to them. See
// bots/whole-improve-loop/main.bot and bots/branch-improve-loop/main.bot.
var reviewLoopBots = []string{
	"feature-dev",
	"docs-refresh",
	"secured-renovacy",
}

func TestReviewLoopBotsUseConditionTopology(t *testing.T) {
	for _, bot := range reviewLoopBots {
		bot := bot
		t.Run(bot, func(t *testing.T) {
			path := "./" + bot + "/main.bot"
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}
			src := string(raw)

			// No `round_robin` router DIRECTIVE (comments mentioning the
			// word round_robin are fine — we match the `mode:` line only).
			if strings.Contains(src, "mode: round_robin") {
				t.Errorf("%s: found a `mode: round_robin` router — the review loop must use a `condition` router so MONO can gate the second family (round_robin ignores `when` guards). See pkg/reviewtopology.", bot)
			}

			// Topology vars must be declared so the resolver can inject them
			// and the router edges / stop expression can read them.
			if !strings.Contains(src, "review_mode: string") {
				t.Errorf("%s: missing `review_mode: string` var — required for mono/dual topology injection.", bot)
			}
			if !strings.Contains(src, "mono_family: string") {
				t.Errorf("%s: missing `mono_family: string` var — required for the mono family selection.", bot)
			}

			// The gpt reviewer edge must be family-gated (the parity/mono
			// guard). Cheap structural check: an `if(vars.review_mode` guard
			// referencing mono_family is present on a reviewer_gpt edge.
			if !strings.Contains(src, "reviewer_gpt when \"if(vars.review_mode") {
				t.Errorf("%s: reviewer_gpt edge is not topology-guarded (expected `-> reviewer_gpt when \"if(vars.review_mode …\"`).", bot)
			}
		})
	}
}
