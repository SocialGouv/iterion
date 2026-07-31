package e2e

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestOnlyAPublishedNodeLeavesAnArtifact pins the contract the run-to-run
// hand-off rests on, against the REAL engine.
//
// A bot declares in its manifest which node another run may start from
// (`produces:`), and the resolver loads that node's artifact. But the engine
// persists an artifact only for a node carrying `publish:`
// (runtime.persistArtifactIfPublished returns early otherwise). A producer
// naming a node that publishes nothing therefore hands over NOTHING — with the
// node executed, the workflow finished, and no error anywhere.
//
// That is exactly how the hand-off shipped, and it took a live run on a real
// pull request to notice: every unit test wrote the artifact by hand, so the
// one thing that had to be true in production was the one thing never asserted.
func TestOnlyAPublishedNodeLeavesAnArtifact(t *testing.T) {
	wf := compileFixture(t, "handoff_publish_mini.bot")
	exec := newScenarioExecutor()
	out := map[string]any{
		"findings":       []any{map[string]any{"title": "t", "file": "a.go", "severity": "high"}},
		"total_findings": 1,
		"_tokens":        10, "_cost_usd": 0.001,
	}
	exec.on("published_agent", func(map[string]any) (map[string]any, error) { return out, nil })
	exec.on("unpublished_agent", func(map[string]any) (map[string]any, error) { return out, nil })
	// The harness stubs tool nodes too, so give it the shape the schema declares;
	// what is under test is the publish hook, not the shell.
	exec.on("published_tool", func(map[string]any) (map[string]any, error) {
		return map[string]any{"reviewed_sha": "cafe1234", "_tokens": 0, "_cost_usd": 0.0}, nil
	})

	s := tmpStore(t)
	const runID = "e2e-handoff-publish"
	if err := runtime.New(wf, s, exec).Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	ctx := context.Background()
	if r, _ := s.LoadRun(ctx, runID); r.Status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished", r.Status)
	}

	// Every node kind the catalog declares as a hand-off source, read by exactly
	// the call the hand-off makes. compute and tool run their own completion
	// paths, so covering only the agent would leave the fallback and the anchor
	// — the two the resolver falls back on — unasserted.
	for node, want := range map[string]string{
		"published_agent":   "total_findings",
		"published_compute": "family_summary",
		"published_tool":    "reviewed_sha",
	} {
		art, err := s.LoadLatestArtifact(ctx, runID, node)
		if err != nil || art == nil {
			t.Errorf("%s declares `publish:` but left no artifact (%v) — every hand-off from it resolves to nothing", node, err)
			continue
		}
		if _, ok := art.Data[want]; !ok {
			t.Errorf("%s: artifact does not carry the node's output (%v missing): %v", node, want, art.Data)
		}
	}

	// And the node that ran identically but declared no `publish:` left none —
	// the silent miss this test exists to make loud.
	if got, err := s.LoadLatestArtifact(ctx, runID, "unpublished_agent"); err == nil && got != nil {
		t.Error("a node without `publish:` left an artifact — the guard in bots/handoff_declarations_test.go would then be pointless; re-check which contract is true")
	}
}
