package supervise

import (
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func evb(seq int64, typ store.EventType, node, branch string) *store.Event {
	return &store.Event{Seq: seq, Type: typ, NodeID: node, BranchID: branch, Timestamp: time.Now()}
}

// Both observer transports drop events non-blockingly on a full buffer,
// and evaluate() blocks the run() goroutine for the whole LLM call — so a
// node_finished CAN be lost while the coordinator is busy. Within one
// branch the engine executes nodes sequentially, so the next
// node_started on the same branch is proof the previous node finished:
// active-node tracking must self-heal on it instead of keeping the
// watched node armed forever.
func TestDroppedNodeFinishedSelfHealsOnNextStart(t *testing.T) {
	c := newBareCoordinator(t, Spec{Watches: []string{"a"}}, &stubEval{}, nil)
	c.ingest(evb(1, store.EventNodeStarted, "a", ""))
	if !c.armed() {
		t.Fatal("watched active node should arm")
	}
	// node_finished for "a" was dropped by a full subscriber buffer.
	c.ingest(evb(2, store.EventNodeStarted, "b", ""))
	if c.armed() {
		t.Fatal("same-branch node_started must supersede the previous node: the watched node cannot still be active")
	}
	if c.lastWatchedActive != "" {
		t.Fatalf("lastWatchedActive = %q; want empty after supersession", c.lastWatchedActive)
	}
}

// A stale node_finished (late redelivery of the superseded node) must
// not clear the branch's CURRENT node.
func TestStaleNodeFinishedDoesNotClearCurrentNode(t *testing.T) {
	c := newBareCoordinator(t, Spec{Watches: []string{"b"}}, &stubEval{}, nil)
	c.ingest(evb(1, store.EventNodeStarted, "a", ""))
	c.ingest(evb(2, store.EventNodeStarted, "b", ""))
	c.ingest(evb(3, store.EventNodeFinished, "a", ""))
	if !c.armed() {
		t.Fatal("a stale finished for a superseded node must not disarm the branch's current watched node")
	}
}

// Parallel branches carry distinct branch ids on their node events; one
// branch's activity must not perturb another's.
func TestParallelBranchesTrackIndependently(t *testing.T) {
	c := newBareCoordinator(t, Spec{Watches: []string{"campaign"}}, &stubEval{}, nil)
	c.ingest(evb(1, store.EventNodeStarted, "campaign", "b1"))
	c.ingest(evb(2, store.EventNodeStarted, "sibling", "b2"))
	c.ingest(evb(3, store.EventNodeFinished, "sibling", "b2"))
	if !c.armed() {
		t.Fatal("supervisor disarmed by an unrelated branch's start+finish while the watched node is still active")
	}
	if c.lastWatchedActive != "campaign" {
		t.Fatalf("lastWatchedActive = %q; want campaign", c.lastWatchedActive)
	}
	c.ingest(evb(4, store.EventNodeFinished, "campaign", "b1"))
	if c.armed() {
		t.Fatal("watched node finished; supervisor must disarm")
	}
}
