package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// The run's session ledger — the background work that died with a CLI
// session's last process, per session id — must reach every node the run
// executes, trunk and branches alike, and survive every checkpoint: a later
// process of the session (a resumed run, on another replica) tells the agent
// the work is gone only if it can still read the entry.

// ctxExecutor is the stub executor with ctx-aware handlers: a delegate reads
// and settles the ledger through the ctx the engine hands it.
type ctxExecutor struct {
	*stubExecutor
	mu    sync.Mutex
	ctxFn map[string]func(context.Context, map[string]any) (map[string]any, error)
}

func newCtxExecutor() *ctxExecutor {
	return &ctxExecutor{stubExecutor: newStubExecutor(), ctxFn: map[string]func(context.Context, map[string]any) (map[string]any, error){}}
}

func (c *ctxExecutor) onCtx(nodeID string, fn func(context.Context, map[string]any) (map[string]any, error)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ctxFn[nodeID] = fn
}

func (c *ctxExecutor) Execute(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
	c.mu.Lock()
	fn, ok := c.ctxFn[node.NodeID()]
	c.mu.Unlock()
	if ok {
		return fn(ctx, input)
	}
	return c.stubExecutor.Execute(ctx, node, input)
}

// settleAs is what a claude_code call does at the end of its process.
func settleAs(sessionID string, terminated ...string) func(context.Context, map[string]any) (map[string]any, error) {
	return func(ctx context.Context, _ map[string]any) (map[string]any, error) {
		l := model.SessionLedgerFromContext(ctx)
		if l == nil {
			return nil, errors.New("no session ledger on the node's ctx")
		}
		l.Settle(sessionID, nil, terminated)
		return map[string]any{"_session_id": sessionID}, nil
	}
}

func TestFanOutBranchesSettleIntoTheRunsOneLedger(t *testing.T) {
	wf := fanOutWorkflow(ir.AwaitWaitAll)
	exec := newCtxExecutor()
	exec.on("entry", func(map[string]any) (map[string]any, error) { return map[string]any{"summary": "s"}, nil })
	exec.onCtx("agent_a", settleAs("sess-a", "a-agent (local_agent, t1)"))
	exec.onCtx("agent_b", settleAs("sess-b", "b-agent (local_agent, t2)"))
	var seen map[string][]string
	exec.onCtx("finalize", func(ctx context.Context, _ map[string]any) (map[string]any, error) {
		l := model.SessionLedgerFromContext(ctx)
		if l == nil {
			return nil, errors.New("no session ledger on the trunk's ctx")
		}
		seen = map[string][]string{"sess-a": l.Terminated("sess-a"), "sess-b": l.Terminated("sess-b")}
		return map[string]any{"result": "ok"}, nil
	})
	s := tmpStore(t)
	if err := New(wf, s, exec).Run(context.Background(), "run-ledger-fanout", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	want := map[string][]string{"sess-a": {"a-agent (local_agent, t1)"}, "sess-b": {"b-agent (local_agent, t2)"}}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("the trunk read %v, want both branches' entries — a branch wrote to a ledger of its own", seen)
	}
	r, err := s.LoadRun(context.Background(), "run-ledger-fanout")
	if err != nil {
		t.Fatal(err)
	}
	if r.Checkpoint != nil && len(r.Checkpoint.SessionLedger) != 2 {
		t.Fatalf("checkpoint ledger = %+v, want both entries", r.Checkpoint.SessionLedger)
	}
}

func TestTheSessionLedgerSurvivesAFailedRunAndItsResume(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "ledger_resume",
		Entry: "first",
		Nodes: map[string]ir.Node{
			"first":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "first"}},
			"second": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "second"}},
			"done":   &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{{From: "first", To: "second"}, {From: "second", To: "done"}},
	}
	dead := "auditor (local_agent, t1)"
	calls := 0
	var resumed []string
	exec := newCtxExecutor()
	exec.onCtx("first", settleAs("sess-1", dead))
	exec.onCtx("second", func(ctx context.Context, _ map[string]any) (map[string]any, error) {
		calls++
		if calls == 1 {
			return nil, errors.New("the runner went away")
		}
		resumed = model.SessionLedgerFromContext(ctx).Terminated("sess-1")
		return map[string]any{"text": "ok"}, nil
	})
	s := tmpStore(t)
	ctx := context.Background()
	if err := New(wf, s, exec).Run(ctx, "run-ledger-resume", nil); err == nil {
		t.Fatal("Run: want the injected failure")
	}
	r, err := s.LoadRun(ctx, "run-ledger-resume")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %s, want failed_resumable", r.Status)
	}
	want := []store.SessionLedgerEntry{{SessionID: "sess-1", Tasks: []string{dead}}}
	if r.Checkpoint == nil || !reflect.DeepEqual(r.Checkpoint.SessionLedger, want) {
		t.Fatalf("failure checkpoint ledger = %+v, want %+v", r.Checkpoint, want)
	}
	// A fresh engine: nothing but the checkpoint carries the entry across.
	if err := New(wf, s, exec).Resume(ctx, "run-ledger-resume", nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !reflect.DeepEqual(resumed, []string{dead}) {
		t.Fatalf("the resumed run read %v for sess-1, want %v", resumed, []string{dead})
	}
}

func TestTheSessionLedgerSurvivesAPause(t *testing.T) {
	wf := interactionWorkflow(ir.InteractionHuman)
	wf.Nodes["worker"] = &ir.AgentNode{
		BaseNode:          ir.BaseNode{ID: "worker"},
		Session:           ir.SessionInherit,
		InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman},
	}
	dead := "sleeper (local_agent, t1)"
	calls := 0
	var resumed []string
	exec := newCtxExecutor()
	exec.onCtx("worker", func(ctx context.Context, _ map[string]any) (map[string]any, error) {
		calls++
		l := model.SessionLedgerFromContext(ctx)
		if calls == 1 {
			l.Settle("sess-ask", nil, []string{dead})
			return nil, &model.ErrNeedsInteraction{
				NodeID:    "worker",
				Questions: map[string]any{delegate.AskUserQuestionKey: "ok?"},
				SessionID: "sess-ask",
				Backend:   "claude_code",
			}
		}
		resumed = l.Terminated("sess-ask")
		return map[string]any{"text": "done", "_tokens": 1}, nil
	})
	s := tmpStore(t)
	ctx := context.Background()
	if err := New(wf, s, exec).Run(ctx, "run-ledger-pause", nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("want paused, got %v", err)
	}
	if err := New(wf, s, exec).Resume(ctx, "run-ledger-pause", map[string]any{delegate.AskUserQuestionKey: "yes"}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if !reflect.DeepEqual(resumed, []string{dead}) {
		t.Fatalf("the resumed session read %v, want %v — it would wait on dead work", resumed, []string{dead})
	}
}

// The checkpoint's ledger is sorted by session id: the same ledger writes the
// same bytes, whatever the map's order.
func TestTheCheckpointsSessionLedgerIsSortedBySessionID(t *testing.T) {
	entries := map[string][]string{}
	for i := 19; i >= 0; i-- {
		entries[fmt.Sprintf("s%02d", i)] = []string{fmt.Sprintf("task %d (local_agent, t%d)", i, i)}
	}
	got := snapshotSessionLedger(delegate.NewSessionLedger(entries))
	if len(got) != 20 {
		t.Fatalf("entries = %d, want 20", len(got))
	}
	for i, e := range got {
		if want := fmt.Sprintf("s%02d", i); e.SessionID != want {
			t.Fatalf("entry %d = %q, want %q: the checkpoint's ledger is not sorted by session id", i, e.SessionID, want)
		}
	}
}
