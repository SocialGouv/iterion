package runner

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/queue"
	"github.com/SocialGouv/iterion/pkg/store"
)

// #661 — the run releases its key's slot the moment its last model node
// finishes and takes it back the moment one starts: a sixty-minute
// tool-only verify gate between two agent passes holds a slot for nobody.
func TestCredSlotTracker_IdleBetweenModelNodes(t *testing.T) {
	var writes []*time.Time
	now := time.Date(2026, 9, 4, 14, 0, 0, 0, time.UTC)
	tr := &credSlotTracker{
		isLLM: func(id string) bool { return id == "campaign" || id == "judge" },
		set:   func(at *time.Time) { writes = append(writes, at) },
		now:   func() time.Time { return now },
	}
	ev := func(typ store.EventType, node string) store.Event { return store.Event{Type: typ, NodeID: node} }

	// Claim → first model node: the run already counts (marker nil), so
	// the start writes nothing.
	tr.observe(ev(store.EventNodeStarted, "campaign"))
	if len(writes) != 0 {
		t.Fatalf("a model node starting on a counting run wrote %d marker(s)", len(writes))
	}
	// Its finish releases the slot, once.
	tr.observe(ev(store.EventNodeFinished, "campaign"))
	if len(writes) != 1 || writes[0] == nil || !writes[0].Equal(now) {
		t.Fatalf("finish of the last model node: writes = %v, want one idle marker at %s", writes, now)
	}
	// Tool-only work in between changes nothing.
	tr.observe(ev(store.EventNodeStarted, "verify_run"))
	tr.observe(ev(store.EventNodeFinished, "verify_run"))
	tr.observe(ev(store.EventLLMRequest, "campaign"))
	if len(writes) != 1 {
		t.Fatalf("tool-only events wrote markers: %v", writes)
	}
	// The loop comes back to the agent: the slot is taken again.
	tr.observe(ev(store.EventNodeStarted, "campaign"))
	if len(writes) != 2 || writes[1] != nil {
		t.Fatalf("a model node restarting on an idle run: writes = %v, want a nil (cleared) marker", writes)
	}
	// Two model nodes in parallel branches: the slot is held until BOTH
	// have finished.
	tr.observe(ev(store.EventNodeStarted, "judge"))
	tr.observe(ev(store.EventNodeFinished, "campaign"))
	if len(writes) != 2 {
		t.Fatalf("one of two concurrent model nodes finishing released the slot: %v", writes)
	}
	tr.observe(ev(store.EventNodeFinished, "judge"))
	if len(writes) != 3 || writes[2] == nil {
		t.Fatalf("both model nodes finished: writes = %v, want an idle marker", writes)
	}
	// A stray finish (a node the tracker never saw start) cannot drive the
	// count negative or double-write.
	tr.observe(ev(store.EventNodeFinished, "judge"))
	if len(writes) != 3 {
		t.Fatalf("a stray finish wrote a marker: %v", writes)
	}
}

// slotSpyStore records the idle marker the runner writes.
type slotSpyStore struct {
	store.RunStore
	runID  string
	writes []*time.Time
	tenant string
}

func (s *slotSpyStore) SetRunLLMIdle(ctx context.Context, runID string, idleSince *time.Time) error {
	s.runID = runID
	s.writes = append(s.writes, idleSince)
	s.tenant, _ = store.TenantFromContext(ctx)
	return nil
}

// The wiring: the observer resolves LLM-ness from the compiled workflow,
// writes through the store under the run's tenant, and is absent for a run
// that sealed no credentials (nothing stamped, nothing to release).
func TestCredSlotObserver_WiresWorkflowAndStore(t *testing.T) {
	spy := &slotSpyStore{}
	r := &Runner{cfg: Config{Store: spy, Logger: iterlog.Nop()}}
	wf := &ir.Workflow{
		Name:  "campaign",
		Entry: "campaign",
		Nodes: map[string]ir.Node{
			"campaign": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "campaign"}},
			"verify":   &ir.ToolNode{BaseNode: ir.BaseNode{ID: "verify"}},
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
	}
	msg := &queue.RunMessage{RunID: "run-661", TenantID: "team-7", OwnerID: "u1", SecretsRef: "rs-1"}

	if r.credSlotObserver(context.Background(), &queue.RunMessage{RunID: "no-creds"}, wf, iterlog.Nop()) != nil {
		t.Fatal("a run with no sealed credentials got a slot observer")
	}
	obs := r.credSlotObserver(context.Background(), msg, wf, iterlog.Nop())
	if obs == nil {
		t.Fatal("no observer for a run with sealed credentials")
	}
	obs.observe(store.Event{Type: store.EventNodeStarted, NodeID: "campaign"})
	obs.observe(store.Event{Type: store.EventNodeFinished, NodeID: "campaign"})
	obs.observe(store.Event{Type: store.EventNodeStarted, NodeID: "verify"})
	if len(spy.writes) != 1 || spy.writes[0] == nil {
		t.Fatalf("writes = %v, want one idle marker after the agent node finished", spy.writes)
	}
	if spy.runID != "run-661" || spy.tenant != "team-7" {
		t.Fatalf("marker written for run %q under tenant %q, want run-661 / team-7", spy.runID, spy.tenant)
	}
	obs.observe(store.Event{Type: store.EventNodeStarted, NodeID: "campaign"})
	if len(spy.writes) != 2 || spy.writes[1] != nil {
		t.Fatalf("writes = %v, want the marker cleared when the agent node restarts", spy.writes)
	}
}
