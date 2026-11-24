package routing

import (
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// EvaluateAutoMerge: the ONE reading of the launch intent. Each branch
// below is a witness — revert the reading's precondition and its own
// test reddens on its own assertion.

func TestEvaluateAutoMerge_LandsOnlyWhatItWasLaunchedToLand(t *testing.T) {
	finished := &store.Run{ID: "r", Status: store.RunStatusFinished, AutoMerge: true}
	none := &store.Run{ID: "r", Status: store.RunStatusFinished, AutoMerge: true, MergeInto: "none"}
	noneSpaced := &store.Run{ID: "r", Status: store.RunStatusFinished, AutoMerge: true, MergeInto: " none "}
	failed := &store.Run{ID: "r", Status: store.RunStatusFailed, AutoMerge: true}
	cancelled := &store.Run{ID: "r", Status: store.RunStatusCancelled, AutoMerge: true}
	notArmed := &store.Run{ID: "r", Status: store.RunStatusFinished, AutoMerge: false}
	// The finalize REFUSED to merge: a wip-banked HEAD (or no merge
	// target at all — a detached HEAD) is recorded merge_status=skipped,
	// and the intent must not override that refusal.
	skipped := &store.Run{ID: "r", Status: store.RunStatusFinished, AutoMerge: true, MergeStatus: store.MergeStatusSkipped}

	cases := []struct {
		name   string
		run    *store.Run
		intent bool
	}{
		{"a finished auto_merge run lands", finished, true},
		{"an explicit none opts out", none, false},
		{"the opt-out tolerates spacing", noneSpaced, false},
		{"a failed run keeps its own continuation", failed, false},
		{"a cancelled run is an operator's stop", cancelled, false},
		{"an unarmed run is nobody's to land", notArmed, false},
		{"a finalize refusal is not overridden", skipped, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, ok := EvaluateAutoMerge(tc.run)
			if ok != tc.intent {
				t.Fatalf("intent = %t, want %t (verdict %+v)", ok, tc.intent, v)
			}
			if ok && v.Decision != DecisionMerge {
				t.Fatalf("decision = %q, want merge", v.Decision)
			}
			if ok && !strings.Contains(v.Reason, "launch intent auto_merge") {
				t.Fatalf("reason = %q, want the intent named", v.Reason)
			}
			if ok && v.PolicyHash != "" {
				t.Fatalf("an intent verdict pins no contract hash, got %q", v.PolicyHash)
			}
		})
	}
}
