package notify

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// Run.ArtifactIndex is a map, so "the first final_answer found" is not a
// fact: a run with two answer-carrying nodes posted whichever one the
// range happened to yield, and a redelivery of the SAME finished run
// could post the other. These tests judge the resolved answer over many
// resolutions and with the index built in both orders, because a test
// that can only redden in one walk order is flaky, not a witness.

// resolutionRounds makes a map-order verdict improbable rather than
// unlucky: with three candidate nodes, an unordered scan agrees with the
// expected answer 50 times running with probability (1/3)^50.
const resolutionRounds = 50

// seedAnswerArtifacts creates a run with no CallbackAnswerNode and
// writes one final_answer artifact per entry, each stamped with its own
// written_at. nodeOrder decides the order the artifacts are WRITTEN in,
// which is the only order a caller controls.
func seedAnswerArtifacts(t *testing.T, st *store.FilesystemRunStore, runID string, nodeOrder []string, answers map[string]string, at map[string]time.Time) *store.Run {
	t.Helper()
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, runID, "clarify", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	for _, nodeID := range nodeOrder {
		art := &store.Artifact{
			RunID:     runID,
			NodeID:    nodeID,
			Version:   1,
			Data:      map[string]any{DefaultAnswerField: answers[nodeID]},
			WrittenAt: at[nodeID],
		}
		if err := st.WriteArtifact(ctx, art); err != nil {
			t.Fatalf("WriteArtifact %s: %v", nodeID, err)
		}
	}
	run, err := st.LoadRun(ctx, runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if len(run.ArtifactIndex) != len(nodeOrder) {
		t.Fatalf("artifact index holds %d nodes, want %d: the fixture did not land", len(run.ArtifactIndex), len(nodeOrder))
	}
	return run
}

// TestTheLastWrittenAnswerWinsWhateverTheWalkOrder is the witness for the
// defect: three nodes carry an answer, and the one written last must win
// every time, with the artifacts seeded in both directions.
func TestTheLastWrittenAnswerWinsWhateverTheWalkOrder(t *testing.T) {
	base := time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)
	answers := map[string]string{
		"aggregate": "the aggregate draft",
		"middle":    "the middle pass",
		"zz-final":  "the answer the receiver is promised",
	}
	at := map[string]time.Time{
		"aggregate": base,
		"middle":    base.Add(time.Minute),
		"zz-final":  base.Add(2 * time.Minute),
	}
	// "aggregate" sorts first and "zz-final" last, so neither a sorted
	// walk nor a reverse one can fake this by accident: the expected
	// winner is the LAST WRITTEN, not an extreme of the id order.
	forward := []string{"aggregate", "middle", "zz-final"}
	backward := []string{"zz-final", "middle", "aggregate"}

	for _, tc := range []struct {
		name  string
		order []string
	}{
		{name: "written in id order", order: forward},
		{name: "written in reverse id order", order: backward},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := newTestStore(t)
			run := seedAnswerArtifacts(t, st, "run-order", tc.order, answers, at)
			n := &Notifier{}
			for i := 0; i < resolutionRounds; i++ {
				answer, node := n.resolveFinalAnswer(context.Background(), st, run)
				if node != "zz-final" || answer != answers["zz-final"] {
					t.Fatalf("round %d resolved node %q answer %q, want the last-written node %q: the scan has no ordering key", i, node, answer, "zz-final")
				}
			}
		})
	}
}

// TestAnswersStampedInTheSameInstantResolveByNodeID pins the tie-break,
// which is the half a timestamp alone does not decide: two artifacts can
// carry the same written_at (a coarse clock, a replayed fixture), and the
// verdict still has to be the same on every delivery.
func TestAnswersStampedInTheSameInstantResolveByNodeID(t *testing.T) {
	same := time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)
	answers := map[string]string{
		"a-node": "the answer of the first id",
		"b-node": "the answer of the second id",
		"c-node": "the answer of the third id",
	}
	at := map[string]time.Time{"a-node": same, "b-node": same, "c-node": same}

	for _, order := range [][]string{
		{"a-node", "b-node", "c-node"},
		{"c-node", "b-node", "a-node"},
		{"b-node", "c-node", "a-node"},
	} {
		st := newTestStore(t)
		run := seedAnswerArtifacts(t, st, "run-tie", order, answers, at)
		n := &Notifier{}
		for i := 0; i < resolutionRounds; i++ {
			answer, node := n.resolveFinalAnswer(context.Background(), st, run)
			if node != "a-node" || answer != answers["a-node"] {
				t.Fatalf("written order %v, round %d: resolved %q (%q), want the lowest node id on a tie", order, i, node, answer)
			}
		}
	}
}

// TestThePinnedAnswerNodeStillWinsOverALaterOne keeps the ordering key
// from quietly overriding an explicit choice: a run that names its answer
// node gets that node's answer even when another node wrote later.
func TestThePinnedAnswerNodeStillWinsOverALaterOne(t *testing.T) {
	base := time.Date(2026, 10, 29, 8, 0, 0, 0, time.UTC)
	answers := map[string]string{"pinned": "the named node's answer", "later": "written afterwards"}
	at := map[string]time.Time{"pinned": base, "later": base.Add(time.Hour)}

	st := newTestStore(t)
	run := seedAnswerArtifacts(t, st, "run-pinned", []string{"pinned", "later"}, answers, at)
	run.CallbackAnswerNode = "pinned"

	n := &Notifier{}
	answer, node := n.resolveFinalAnswer(context.Background(), st, run)
	if node != "pinned" || answer != answers["pinned"] {
		t.Fatalf("resolved %q (%q), want the named node's answer", node, answer)
	}
}

// TestNoAnswerArtifactStaysSilent holds the legitimate empty outcome: the
// ordering key must not invent an answer where none exists.
func TestNoAnswerArtifactStaysSilent(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, "run-silent", "clarify", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.WriteArtifact(ctx, &store.Artifact{
		RunID: "run-silent", NodeID: "plan", Version: 1,
		Data: map[string]any{"plan": "no answer field here"}, WrittenAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("WriteArtifact: %v", err)
	}
	run, err := st.LoadRun(ctx, "run-silent")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	n := &Notifier{}
	if answer, node := n.resolveFinalAnswer(ctx, st, run); answer != "" || node != "" {
		t.Fatalf("resolved %q (%q), want silence", node, answer)
	}
}
