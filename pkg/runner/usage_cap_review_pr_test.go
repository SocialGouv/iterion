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

// Read on the REAL review-pr bot: a `single` review while the Anthropic
// window is spent, with NO dial set at all.
//
// The roles are what make this hold (ADR-169): `single` takes `reviewer` —
// claw + an `openai/…` model — and the merge step `converge` on the same
// claw route, while `reviewer_alternate` (the claude slot) is reached only
// by the `paired` fan-out. The `pacer` supervisor watches
// `reviewer_alternate` alone, so it is on no route this run takes either (a
// coordinator evaluates only while a watched node is active), and
// `pacer_gpt` resolves off the Anthropic wire. Nothing the run can reach
// spends the capped subscription, so it starts under either cap mode with no
// deployment profile.
//
// A review runs where its credential funds it: the pod's OpenAI key here,
// staged because the package's TestMain isolates the host's providers and the
// spawn keeps a supervisor only where a credential funds it
// (runview.FundedSupervisorSpecs).
func TestUsageCapPreflight_SingleReviewOnTheReviewPrBot(t *testing.T) {
	blankAnthropicWireEnv(t)
	blankReviewPrDials(t)
	t.Setenv("OPENAI_API_KEY", "sk-openai-pod")
	wf := compileCatalogBot(t, "review-pr")
	single := map[string]any{"review_mode": "single", "review_tier": "guard"}
	ctx := forfaitBesidePinnedKeys(t)
	resets := time.Now().UTC().Add(30 * time.Hour)
	hard := cappedWire(t, usagecap.WindowSevenDay, 0.92, resets) // the test policy reads the week HARD
	soft := cappedWire(t, usagecap.WindowFiveHour, 0.95, resets) // and the five-hour window SOFT

	open := wf.LaunchVarEdgeFilter(single)
	for _, id := range []string{"reviewer_alternate", "reviewer_glance"} {
		if reaches(wf, open, id) {
			t.Errorf("%s is on a single run's topology — only the paired fan-out reaches the alternate, and this tier routes past the glance reviewer", id)
		}
	}
	for _, id := range []string{"reviewer", "converge"} {
		if !reaches(wf, open, id) {
			t.Errorf("%s is off a single run's topology — the prune may only drop what the launch excluded", id)
		}
	}

	// The supervisor walk: the `pacer` watches the node this launch excluded,
	// so it is not a route the run takes; `pacer_gpt` is, and the cap does
	// not meter it.
	sups := preflightSupervisors(ctx, wf, &queue.RunMessage{RunID: "run-sups", Vars: single}, decidedVarsForLaunch(wf, single), open, iterlog.Nop())
	if len(sups) != 1 || sups[0].id != supervisorRouteIDPrefix+"pacer_gpt" {
		t.Fatalf("the supervisor walk of a single review is %v, want only supervisor:pacer_gpt — the claude pacer watches no node this run reaches", sups)
	}
	if _, metered := usageCapCredKeys(ctx, &queue.RunMessage{RunID: "run-sups"}).supervisorKey(ctx, sups[0].spec, sups[0].hint); metered {
		t.Errorf("pacer_gpt resolves to %q, which the cap meters — the gpt pacer must leave the Anthropic wire", sups[0].spec)
	}

	// The whole point: no env, no integration override, both cap modes.
	for mode, caps := range map[string]usagecap.Store{"hard": hard, "soft": soft} {
		if err := preflightWithVars(t, caps, ctx, wf, single); err != nil {
			t.Errorf("a single review parked under the %s cap with no dial set: %v — single means no Anthropic wire, merge and pacer included", mode, err)
		}
	}

	t.Run("a paired review rides the wire and parks on it", func(t *testing.T) {
		paired := map[string]any{"review_mode": "paired", "review_tier": "guard"}
		openPaired := wf.LaunchVarEdgeFilter(paired)
		if !reaches(wf, openPaired, "reviewer_alternate") || !reaches(wf, openPaired, "reviewer") {
			t.Fatal("a paired review does not reach both reviewers — the fan-out is what paired spends on")
		}
		// The alternate reviewer, the merge step's own route and the claude
		// pacer are on this run's topology; a soft cap stops nothing in
		// flight, so the pre-flight parks it.
		if err := preflightWithVars(t, soft, ctx, wf, paired); err == nil {
			t.Error("a paired review started on a soft-capped wire — its alternate reviewer and pacer both spend it")
		}
	})

	t.Run("an operator dial still wins on the merge step", func(t *testing.T) {
		// The merge step's own three dials, moved together onto the capped
		// wire: an explicit choice is honoured, never silently replaced, and
		// the pre-flight reads it.
		t.Setenv("ITERION_VIBE_BACKEND_EMIT_GPT", "claude_code")
		t.Setenv("ITERION_VIBE_MODEL_EMIT_GPT", "claude-opus-5-5")
		t.Setenv("ITERION_VIBE_PROVIDER_EMIT_GPT", "anthropic")
		if err := preflightWithVars(t, soft, ctx, wf, single); err == nil {
			t.Error("a single review whose merge dials were pointed AT the wire started under a soft cap — the dials must still decide the route")
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
		{"a single launch arms the reviewer pacer alone", map[string]any{"review_mode": "single", "review_tier": "guard"}, 1},
		// The glance tier routes to `reviewer_glance`, which neither pacer
		// watches: a glance review arms none (unchanged by the role rename).
		{"a single glance launch arms no pacer", map[string]any{"review_mode": "single", "review_tier": "glance"}, 0},
		{"the audit tier arms both", map[string]any{"review_mode": "single", "review_tier": "audit"}, 2},
		{"a paired launch arms both", map[string]any{"review_mode": "paired"}, 2},
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
