package trigger

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// A FAILED run's checkpoint node is the failure anchor, not stale pause
// evidence. usernotify renders it ("The run failed at node X"), so gating
// nodeID on IsPaused() alongside interactionID would silently degrade
// every run-failed push to the generic body. Only the interaction id is a
// consumable pause pointer — that one must NOT ride a terminal outcome.
func TestRunOutcomeKeepsFailureNodeAndDropsInteraction(t *testing.T) {
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.CreateRun(ctx, "r-fail", "wf", nil); err != nil {
		t.Fatal(err)
	}
	// Real genealogy for a failed run that once held a pointer: a pause,
	// then a status-only park — the transition normalization
	// (store.CarriesPausePointer) consumes the pointer on the way, so
	// the outcome's interaction gate below is belt-and-braces.
	cp := &store.Checkpoint{NodeID: "implement", InteractionID: "I-stale",
		InteractionQuestions: map[string]any{"q": "?"}}
	if err := s.PauseRun(ctx, "r-fail", cp); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRunStatusCoded(ctx, "r-fail", store.RunStatusFailedResumable,
		"boom at implement", store.FailureExecutionFailed); err != nil {
		t.Fatal(err)
	}

	ev := BuildRunOutcome(ctx, s, "r-fail", nil)
	if node, _ := ev.Payload["node_id"].(string); node != "implement" {
		t.Errorf("run-failed outcome carries node_id=%q, want %q — usernotify renders \"The run failed at node X\" and falls back to the generic body without it", node, "implement")
	}
	if inter, ok := ev.Payload["interaction_id"]; ok {
		t.Errorf("run-failed outcome carries interaction_id=%v — a non-paused outcome must not deep-link to an interaction", inter)
	}
}
