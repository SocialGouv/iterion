package runtime

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// TestPauseDrainRespectsNodeScope pins the pause-time inbox drain to the
// PAUSING node: a supervisor's steering tagged for `campaign` must not
// be folded into an unrelated human node's resume prompt. Before the
// fix, drainOperatorMessagesForPause drained with activeNode == "",
// which matches every message.
func TestPauseDrainRespectsNodeScope(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-pause-drain-scope"
	if _, err := s.CreateRun(ctx, runID, "w", nil); err != nil {
		t.Fatalf("create run: %v", err)
	}
	// One message for another node, one run-scoped, one for the pauser.
	for _, m := range []store.QueuedUserMessage{
		{ID: "m-campaign", Text: "[supervisor persy] keep going", NodeID: "campaign"},
		{ID: "m-run", Text: "run-scoped note", NodeID: ""},
		{ID: "m-review", Text: "for the reviewer", NodeID: "human_review"},
	} {
		if err := s.AppendQueuedMessage(ctx, runID, m); err != nil {
			t.Fatalf("queue %s: %v", m.ID, err)
		}
	}

	eng := New(&ir.Workflow{Name: "w", Nodes: map[string]ir.Node{}, Edges: []*ir.Edge{}, Loops: map[string]*ir.Loop{}}, s, nil)
	texts := eng.drainOperatorMessagesForPause(ctx, runID, "human_review")

	if len(texts) != 2 {
		t.Fatalf("drained %d messages %v; want 2 (run-scoped + the pausing node's)", len(texts), texts)
	}
	for _, txt := range texts {
		if txt == "[supervisor persy] keep going" {
			t.Fatal("a message scoped to another node leaked into the pause drain")
		}
	}

	// The foreign-node message is still pending for its own node.
	pending, err := s.LoadPendingQueuedMessages(ctx, runID)
	if err != nil {
		t.Fatalf("load pending: %v", err)
	}
	if len(pending) != 1 || pending[0].NodeID != "campaign" {
		t.Fatalf("pending after drain = %+v; want only the campaign-scoped message", pending)
	}
}
