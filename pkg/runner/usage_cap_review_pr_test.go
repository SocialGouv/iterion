package runner

import (
	"context"
	"github.com/SocialGouv/iterion/pkg/dsl/unit/unittest"
	"path/filepath"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
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
	u := unittest.Whole(unit.LoadDir(path))
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
		"ITERION_VIBE_MODEL_EMIT_GPT", "ITERION_VIBE_BACKEND_EMIT_GPT", "ITERION_VIBE_PROVIDER_EMIT_GPT",
		"ITERION_VIBE_MODEL_SUPERVISOR", "ITERION_VIBE_MODEL_SUPERVISOR_GPT",
		"ITERION_SUPERVISORS", "ITERION_DEFAULT_SUPERVISOR_MODEL",
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

// decidedVarsForLaunch is the reading the pre-flight's supervisor walk gets —
// the dispatch view's vars minus its late names (usageCapPreflight's own
// `view.DecidedVars()`).
func decidedVarsForLaunch(wf *ir.Workflow, vars map[string]any) map[string]any {
	return model.RouteViewForDispatch(wf, model.ModelOverrides{}, nil, vars).DecidedVars()
}

// #2467, read on the REAL review-pr bot: a mono-GPT review
// (`mono_family=gpt`, `review_mode=mono`, `review_tier=guard`) while the
// Anthropic window is spent, with NO dial set at all.
//
// `mono_family=gpt` is the family of the WHOLE review: the launch-vars prune
// takes both claude reviewers AND the claude merge step off this run's
// topology — each gpt guard holds, and that pre-empts the unconditional
// claude fallback beside it — while `converge_gpt` merges on claw. The
// `pacer` supervisor watches the claude reviewer alone, so it is on no route
// this run takes either (a coordinator evaluates only while a watched node is
// active), and `pacer_gpt` resolves off the Anthropic wire. Nothing the run
// can reach spends the capped subscription, so it starts under either cap
// mode with no deployment profile.
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
	for _, id := range []string{"reviewer_claude", "reviewer_claude_glance", "converge"} {
		if reaches(wf, open, id) {
			t.Errorf("%s is on a mono-gpt run's topology — the gpt guard holds, and it pre-empts the claude fallback", id)
		}
	}
	for _, id := range []string{"reviewer_gpt", "converge_gpt"} {
		if !reaches(wf, open, id) {
			t.Errorf("%s is off a mono-gpt run's topology — the prune may only drop what the launch excluded", id)
		}
	}

	// The supervisor walk: the claude `pacer` watches nodes this launch
	// excluded, so it is not a route the run takes; `pacer_gpt` is, and the
	// cap does not meter it.
	sups := preflightSupervisors(ctx, wf, &queue.RunMessage{RunID: "run-sups", Vars: mono}, decidedVarsForLaunch(wf, mono), open, iterlog.Nop())
	if len(sups) != 1 || sups[0].id != supervisorRouteIDPrefix+"pacer_gpt" {
		t.Fatalf("the supervisor walk of a mono-gpt review is %v, want only supervisor:pacer_gpt — the claude pacer watches no node this run reaches", sups)
	}
	if _, metered := usageCapCredKeys(ctx, &queue.RunMessage{RunID: "run-sups"}).supervisorKey(ctx, sups[0].spec); metered {
		t.Errorf("pacer_gpt resolves to %q, which the cap meters — the gpt pacer must leave the Anthropic wire", sups[0].spec)
	}

	// The whole point of the ticket: no env, no integration override, both
	// cap modes.
	for mode, caps := range map[string]usagecap.Store{"hard": hard, "soft": soft} {
		if err := preflightWithVars(t, caps, ctx, wf, mono); err != nil {
			t.Errorf("a mono-gpt review parked under the %s cap with no dial set: %v — mono_family=gpt means no Anthropic wire, merge and pacer included", mode, err)
		}
	}

	t.Run("the claude family still merges on the wire", func(t *testing.T) {
		claude := map[string]any{"mono_family": "claude", "review_mode": "mono", "review_tier": "guard"}
		openClaude := wf.LaunchVarEdgeFilter(claude)
		if !reaches(wf, openClaude, "converge") {
			t.Error("converge is off a mono-claude run's topology — it is still the merge step of every other family")
		}
		if reaches(wf, openClaude, "converge_gpt") {
			t.Error("converge_gpt is on a mono-claude run's topology — the gpt merge step is behind the gpt guard")
		}
		// Unchanged from before this ticket: the claude reviewer, the claude
		// merge step and the claude pacer all ride the capped wire, and a
		// soft cap stops nothing in flight.
		if err := preflightWithVars(t, soft, ctx, wf, claude); err == nil {
			t.Error("a mono-claude review started on a soft-capped wire — its reviewer, merge step and pacer all spend it")
		}
	})

	t.Run("an operator dial still wins on the gpt merge step", func(t *testing.T) {
		// The twin's own three dials, moved together onto the capped wire:
		// an explicit choice is honoured, never silently replaced, and the
		// pre-flight reads it.
		t.Setenv("ITERION_VIBE_BACKEND_EMIT_GPT", "claude_code")
		t.Setenv("ITERION_VIBE_MODEL_EMIT_GPT", "claude-opus-5-5")
		t.Setenv("ITERION_VIBE_PROVIDER_EMIT_GPT", "anthropic")
		if err := preflightWithVars(t, soft, ctx, wf, mono); err == nil {
			t.Error("a mono-gpt review whose merge dials were pointed BACK at the wire started under a soft cap — the dials must still decide the route")
		}
	})
}

// TestReviewPrTheTwoPacersEvaluationBound pins the per-run evaluation bound
// the cost pacer's comment block states, which is the one number an operator
// sizes the supervisor spend on.
//
// review-pr is the only shipped bot with TWO supervisors, one per reviewer
// family, and a coordinator evaluates nothing until one of the nodes it
// watches is active (supervisorArms, the pre-flight's own reading of the
// launch-var topology). So a mono launch arms one pacer and a cross-family
// launch arms both: the bound is max_evals per pacer, and twice that where
// both watch sets are open. Read from the compiled bot and from the function
// that decides it, so a watch set, a budget or a launch guard that moves
// reddens here and the comment is re-read.
func TestReviewPrTheTwoPacersEvaluationBound(t *testing.T) {
	blankReviewPrDials(t)
	wf := compileCatalogBot(t, "review-pr")
	if len(wf.Supervisors) != 2 {
		t.Fatalf("review-pr declares %d supervisor(s), want 2 — the comment's bound is stated over one pacer per family", len(wf.Supervisors))
	}
	const perPacer = 12
	for _, sup := range wf.Supervisors {
		if sup.MaxEvals != perPacer {
			t.Errorf("supervisor %s declares max_evals %d, want %d — the comment states ≤%d evals per pacer", sup.Name, sup.MaxEvals, perPacer, perPacer)
		}
	}
	for _, tc := range []struct {
		name  string
		vars  map[string]any
		armed int
	}{
		{"a mono-gpt launch arms the gpt pacer alone", map[string]any{"mono_family": "gpt", "review_mode": "mono", "review_tier": "guard"}, 1},
		{"a mono-claude launch arms the claude pacer alone", map[string]any{"mono_family": "claude", "review_mode": "mono", "review_tier": "guard"}, 1},
		{"the audit tier arms both", map[string]any{"mono_family": "gpt", "review_tier": "audit"}, 2},
		{"an explicit dual arms both", map[string]any{"mono_family": "gpt", "review_mode": "dual"}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			open := wf.LaunchVarEdgeFilter(tc.vars)
			armed, bound := 0, 0
			for _, sup := range wf.Supervisors {
				if supervisorArms(wf, open, sup.Watches) {
					armed++
					bound += sup.MaxEvals // the bound is the armed pacers' OWN budgets
				}
			}
			if armed != tc.armed {
				t.Fatalf("%d pacer(s) arm on %v, want %d", armed, tc.vars, tc.armed)
			}
			if want := tc.armed * perPacer; bound != want {
				t.Errorf("the run's evaluation bound is %d, want %d — the comment's ≤12 per pacer, ≤24 where both arm", bound, want)
			}
		})
	}
}
