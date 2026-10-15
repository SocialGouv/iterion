package runner

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	"github.com/SocialGouv/iterion/pkg/usagecap"
)

// compileCatalogBot compiles one bot of this repo's own catalog, its imports
// included: the workflow a launch of it publishes, rather than a shape that
// stands for it.
func compileCatalogBot(t *testing.T, bot string) *ir.Workflow {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "bots", bot, "main.bot"))
	if err != nil {
		t.Fatal(err)
	}
	u := unit.LoadDir(path)
	for _, d := range u.Diagnostics {
		if d.Severity == parser.SeverityError {
			t.Fatalf("parse %s: %s", path, d.Error())
		}
	}
	if u.Merged == nil {
		t.Fatalf("parse %s: no AST", path)
	}
	cr := ir.Compile(u.Merged)
	if cr.HasErrors() {
		t.Fatalf("compile %s: %s", path, cr.Diagnostics[0].Error())
	}
	return cr.Workflow
}

// blankReviewPrDials unsets every dial the bot reads, so the run under test is
// the catalog's own defaults whatever the host exports.
func blankReviewPrDials(t *testing.T) {
	t.Helper()
	for _, dial := range []string{
		"ITERION_VIBE_MODEL_CLAUDE", "ITERION_VIBE_BACKEND_CLAUDE", "ITERION_VIBE_PROVIDER_CLAUDE",
		"ITERION_VIBE_MODEL_CLAUDE_GLANCE", "ITERION_VIBE_BACKEND_CLAUDE_GLANCE",
		"ITERION_VIBE_MODEL_GPT", "ITERION_VIBE_BACKEND_GPT", "ITERION_VIBE_PROVIDER_GPT",
		"ITERION_VIBE_MODEL_GPT_GLANCE", "ITERION_VIBE_BACKEND_GPT_GLANCE",
		"ITERION_VIBE_MODEL_EMIT", "ITERION_VIBE_BACKEND_EMIT", "ITERION_VIBE_PROVIDER_EMIT",
		"ITERION_VIBE_MODEL_SUPERVISOR", "ITERION_SUPERVISORS", "ITERION_DEFAULT_SUPERVISOR_MODEL",
	} {
		t.Setenv(dial, "")
	}
}

// cappedWire closes one window for every credential the run holds on the
// Anthropic wire — its forfait and the key pinned for the `anthropic` provider
// — which is what an operator means by "the Anthropic window is spent".
func cappedWire(t *testing.T, window usagecap.Window, util float64, resets time.Time) usagecap.Store {
	t.Helper()
	caps := usagecap.NewMemStore()
	for _, fp := range []string{"fp-forfait", "fp-ant"} {
		if err := caps.Record(context.Background(), platformKey(fp), usagecap.Reading{
			Window: window, Utilization: util, Status: usagecap.StatusWarning,
			ResetsAt: resets, ObservedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	return caps
}

func reaches(wf *ir.Workflow, open ir.EdgeFilter, id string) bool {
	return wf.CanReachVia(open, func(n ir.Node) bool { return n != nil && n.NodeID() == id })
}

// #1828's own scenario, read on the REAL review-pr bot: a mono-GPT review
// (`mono_family=gpt`, `review_mode=mono`, `review_tier=guard`) while the
// Anthropic window is spent.
//
// The launch-vars prune takes both claude reviewers off this run's topology —
// `topology -> reviewer_gpt when vars.mono_family == 'gpt'` holds, and that
// pre-empts the unconditional claude fallback beside it. The bot's two other
// anthropic-wire routes are REAL spend, and they decide whether the run
// starts: the `pacer` supervisor runs on every execution, and `converge`
// merges on every path that reviews anything. Which of the two parks the run
// depends on the cap's mode, and this pins each case — the deployment half of
// the ticket (docs/usage-caps.md).
func TestUsageCapPreflight_MonoGPTOnTheReviewPrBot(t *testing.T) {
	blankAnthropicWireEnv(t)
	blankReviewPrDials(t)
	wf := compileCatalogBot(t, "review-pr")
	mono := map[string]any{"mono_family": "gpt", "review_mode": "mono", "review_tier": "guard"}
	ctx := forfaitBesidePinnedKeys()
	resets := time.Now().UTC().Add(30 * time.Hour)
	hard := cappedWire(t, usagecap.WindowSevenDay, 0.92, resets) // the test policy reads the week HARD
	soft := cappedWire(t, usagecap.WindowFiveHour, 0.95, resets) // and the five-hour window SOFT

	open := wf.LaunchVarEdgeFilter(mono)
	for _, id := range []string{"reviewer_claude", "reviewer_claude_glance"} {
		if reaches(wf, open, id) {
			t.Errorf("%s is on a mono-gpt run's topology — the gpt guard holds, and it pre-empts the claude fallback", id)
		}
	}
	if !reaches(wf, open, "reviewer_gpt") {
		t.Error("reviewer_gpt is off a mono-gpt run's topology — the prune may only drop what the launch excluded")
	}
	if !reaches(wf, open, "converge") {
		t.Error("converge is off the topology — it merges on every path that reviews anything")
	}

	// The supervisor is a route every execution takes, so a capped one parks
	// the run whatever the prune said about the reviewers.
	if err := preflightWithVars(t, hard, ctx, wf, mono); err == nil {
		t.Error("a mono-gpt review started with its `pacer` supervisor on the spent wire — a supervisor spends on every execution")
	}

	t.Run("the supervisor off the wire, under a hard weekly cap", func(t *testing.T) {
		t.Setenv("ITERION_SUPERVISORS", "off")
		// `converge` stays claude_code + anthropic here, and a HARD cap on a
		// claude_code route parks nothing the run can avoid: the bot's
		// empty-diff short-circuit reaches `done` without it, and the mid-run
		// guard stops the call if the run goes that way.
		if err := preflightWithVars(t, hard, ctx, wf, mono); err != nil {
			t.Errorf("a mono-gpt review parked with no capped route on every execution: %v", err)
		}
	})

	t.Run("the merge step still on the wire, under a soft cap", func(t *testing.T) {
		t.Setenv("ITERION_SUPERVISORS", "off")
		// A soft cap stops nothing in flight, so a route the run MAY reach
		// parks it — and `converge` is reachable on every non-empty diff.
		if err := preflightWithVars(t, soft, ctx, wf, mono); err == nil {
			t.Error("a mono-gpt review started with its merge step on a soft-capped wire — nothing would stop that call in flight")
		}
	})

	t.Run("a claw merge step is metered on the credential its model names", func(t *testing.T) {
		t.Setenv("ITERION_SUPERVISORS", "off")
		t.Setenv("ITERION_VIBE_BACKEND_EMIT", "claw")
		t.Setenv("ITERION_VIBE_MODEL_EMIT", "openai/gpt-6-sol")
		// The provider hint is left at its `anthropic` default on purpose:
		// the route still NAMES the wire (`model.AnthropicWireRoutes` lists
		// it), but the cap meters a claw route on the credential its MODEL
		// resolves (`credentialSlotForRoute`) — an openai slot, which this
		// cap does not govern. Clearing the hint is the node's own
		// instruction (`lib/nodes.bot`), for the credential walk, not for
		// this verdict.
		if err := preflightWithVars(t, soft, ctx, wf, mono); err != nil {
			t.Errorf("a mono-gpt review parked for a claw route on an openai model: %v", err)
		}
	})

	// The deployment profile: the supervisor off the wire (either lever) and
	// the merge step on claw + an openai model, its provider hint cleared.
	t.Setenv("ITERION_VIBE_BACKEND_EMIT", "claw")
	t.Setenv("ITERION_VIBE_MODEL_EMIT", "openai/gpt-6-sol")
	t.Setenv("ITERION_VIBE_PROVIDER_EMIT", "auto")
	for _, lever := range []struct{ name, value string }{
		{"ITERION_SUPERVISORS", "off"},
		{"ITERION_VIBE_MODEL_SUPERVISOR", "openai/gpt-6-sol"},
	} {
		t.Run("the full profile with "+lever.name+"="+lever.value, func(t *testing.T) {
			t.Setenv(lever.name, lever.value)
			for mode, caps := range map[string]usagecap.Store{"hard": hard, "soft": soft} {
				if err := preflightWithVars(t, caps, ctx, wf, mono); err != nil {
					t.Errorf("a mono-gpt review parked under the %s cap with every anthropic-wire route off this run: %v", mode, err)
				}
			}
		})
	}
}
