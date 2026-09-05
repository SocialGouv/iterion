package e2e

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// review-pr's dual path (SocialGouv/iterion#741): `fan` fans out to
// `reviewer_claude` / `reviewer_gpt`, both of which the `topology` router
// ALSO reaches directly on the mono path. The branches reconverge at
// `merge_reviews` (`await: best_effort`), and the chain
// merge_reviews -> converge -> pr_gate must execute exactly once per run.
// The event log is the judge, so compute nodes (never seen by the
// executor) count too.
func TestReviewPRDual_CollectorChainFiresOnce(t *testing.T) {
	wf := compileFixtureStubSafe(t, "review-pr/main.bot")
	exec := newScenarioExecutor()
	wireReviewPRStubs(exec)

	s := tmpStore(t)
	runID := "e2e-dual-converges-once"
	if err := runtime.New(wf, s, exec).Run(context.Background(), runID, map[string]any{
		"review_tier": "audit", "mono_family": "claude",
	}); err != nil {
		t.Fatalf("run error: %v", err)
	}
	r, _ := s.LoadRun(context.Background(), runID)
	if r.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished", r.Status)
	}
	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	started := map[string]int{}
	var joins []*store.Event
	for _, evt := range events {
		switch evt.Type {
		case store.EventNodeStarted:
			started[evt.NodeID]++
		case store.EventJoinReady:
			joins = append(joins, evt)
		}
	}
	for _, id := range []string{"reviewer_claude", "reviewer_gpt", "merge_reviews", "converge", "pr_gate"} {
		if started[id] != 1 {
			t.Errorf("node_started(%s) = %d, want exactly 1", id, started[id])
		}
	}
	if got := exec.callCount("converge"); got != 1 {
		t.Errorf("converge executed %d times, want 1 (a second pass is a second LLM call and a second gate post)", got)
	}
	if len(joins) != 1 || joins[0].NodeID != "merge_reviews" || joins[0].Data["strategy"] != "best_effort" {
		t.Errorf("join_ready = %+v, want exactly one at merge_reviews with strategy best_effort", joins)
	}
}

// The two shipped mono/dual topologies elect their declared collector, not
// the reviewer both routers reach.
func TestShippedDualTopologies_ElectTheDeclaredCollector(t *testing.T) {
	for _, tc := range []struct{ bot, router, want string }{
		{"review-pr/main.bot", "fan", "merge_reviews"},
		{"evolve/main.bot", "review_fanout", "aggregate_review"},
	} {
		t.Run(tc.bot, func(t *testing.T) {
			wf := compileFixture(t, tc.bot)
			var fan []*ir.Edge
			for _, e := range wf.Edges {
				if e.From == tc.router {
					fan = append(fan, e)
				}
			}
			if got := ir.ExecBranchConvergencePoint(wf, tc.router, fan); got != tc.want {
				t.Errorf("%s: collector of %s = %q, want %s", tc.bot, tc.router, got, tc.want)
			}
		})
	}
}
