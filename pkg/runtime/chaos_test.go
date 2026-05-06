package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ===========================================================================
// ADR-10 §2: Chaos / fault injection tests
// ===========================================================================

// ---------------------------------------------------------------------------
// failAfterNExecutor — wraps a stubExecutor, fails on the (n+1)th call.
// Mutex-protected for safe use with concurrent fan-out branches.
// ---------------------------------------------------------------------------

type failAfterNExecutor struct {
	inner       *stubExecutor
	mu          sync.Mutex
	count       int
	failAt      int
	failErr     error
	passthrough map[string]bool // nodes that always delegate to inner
}

func newFailAfterN(inner *stubExecutor, n int, err error) *failAfterNExecutor {
	return &failAfterNExecutor{inner: inner, failAt: n, failErr: err, passthrough: map[string]bool{}}
}

func (f *failAfterNExecutor) pass(nodeIDs ...string) *failAfterNExecutor {
	for _, id := range nodeIDs {
		f.passthrough[id] = true
	}
	return f
}

func (f *failAfterNExecutor) Execute(ctx context.Context, node ir.Node, input map[string]interface{}) (map[string]interface{}, error) {
	if f.passthrough[node.NodeID()] {
		return f.inner.Execute(ctx, node, input)
	}

	f.mu.Lock()
	idx := f.count
	f.count++
	f.mu.Unlock()

	if idx >= f.failAt {
		return nil, fmt.Errorf("%w: call #%d to %s", f.failErr, idx, node.NodeID())
	}
	return f.inner.Execute(ctx, node, input)
}

// ---------------------------------------------------------------------------
// Test: executor fails mid fan-out with wait_all → run fails
// ---------------------------------------------------------------------------

func TestChaos_FailMidFanOut_WaitAll(t *testing.T) {
	wf := fanOutWorkflow(ir.AwaitWaitAll)

	stub := newStubExecutor()
	stub.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"summary": "ctx"}, nil
	})
	// failAt=2: entry (0) succeeds, router is not an executor call,
	// first branch (1) succeeds, second branch (2) fails.
	exec := newFailAfterN(stub, 2, errors.New("chaos: provider crash"))

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-chaos-waitall", nil)
	if err == nil {
		t.Fatal("expected error from wait_all with chaos failure")
	}

	r, err := s.LoadRun(context.Background(), "run-chaos-waitall")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFailedResumable {
		t.Errorf("expected status failed_resumable, got %s", r.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: executor fails mid fan-out with best_effort → run succeeds
// ---------------------------------------------------------------------------

func TestChaos_FailMidFanOut_BestEffort(t *testing.T) {
	wf := fanOutWorkflow(ir.AwaitBestEffort)

	stub := newStubExecutor()
	stub.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"summary": "ctx"}, nil
	})
	stub.on("agent_a", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"review": "A ok"}, nil
	})
	stub.on("agent_b", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"review": "B ok"}, nil
	})
	stub.on("finalize", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"result": "partial"}, nil
	})

	// passthrough finalize+done so only branch nodes are subject to the counter.
	exec := newFailAfterN(stub, 2, errors.New("chaos: rate limited")).pass("finalize", "done")

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-chaos-besteffort", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	r, err := s.LoadRun(context.Background(), "run-chaos-besteffort")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected status finished, got %s", r.Status)
	}
}

// ---------------------------------------------------------------------------
// Test: all branches fail (only entry succeeds) → run fails
// ---------------------------------------------------------------------------

func TestChaos_AllBranchesFail(t *testing.T) {
	for _, strategy := range []struct {
		name string
		mode ir.AwaitMode
	}{
		{"wait_all", ir.AwaitWaitAll},
		{"best_effort", ir.AwaitBestEffort},
	} {
		t.Run(strategy.name, func(t *testing.T) {
			wf := fanOutWorkflow(strategy.mode)

			stub := newStubExecutor()
			stub.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
				return map[string]interface{}{"summary": "ctx"}, nil
			})

			// failAt=1: only entry succeeds, both branches fail.
			exec := newFailAfterN(stub, 1, errors.New("chaos: total outage"))

			s := tmpStore(t)
			eng := New(wf, s, exec)

			err := eng.Run(context.Background(), "run-chaos-allfail", nil)
			if err == nil {
				t.Fatal("expected error when all branches fail")
			}

			r, err := s.LoadRun(context.Background(), "run-chaos-allfail")
			if err != nil {
				t.Fatalf("load run: %v", err)
			}
			if r.Status != store.RunStatusFailed && r.Status != store.RunStatusFailedResumable {
				t.Errorf("expected status failed or failed_resumable, got %s", r.Status)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Test: executor fails during a loop iteration
// ---------------------------------------------------------------------------

func TestChaos_FailInLoopIteration(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "chaos_loop_test",
		Entry: "agent",
		Nodes: map[string]ir.Node{
			"agent": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "agent"}},
			"judge": &ir.JudgeNode{BaseNode: ir.BaseNode{ID: "judge"}},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":  &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "agent", To: "judge"},
			{From: "judge", To: "done", Condition: "pass"},
			{From: "judge", To: "agent", Condition: "pass", Negated: true, LoopName: "refine"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops: map[string]*ir.Loop{
			"refine": {Name: "refine", MaxIterations: 5},
		},
	}

	stub := newStubExecutor()
	stub.on("agent", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"code": "v1"}, nil
	})
	stub.on("judge", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"pass": false}, nil // always loop
	})

	// failAt=3: agent(0), judge(1), agent(2) succeed → judge(3) fails.
	exec := newFailAfterN(stub, 3, errors.New("chaos: provider timeout"))

	s := tmpStore(t)
	eng := New(wf, s, exec)

	err := eng.Run(context.Background(), "run-chaos-loop", nil)
	if err == nil {
		t.Fatal("expected error from chaos failure in loop")
	}

	r, err := s.LoadRun(context.Background(), "run-chaos-loop")
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != store.RunStatusFailedResumable {
		t.Errorf("expected status failed_resumable, got %s", r.Status)
	}

	// Verify some nodes were executed before the failure.
	events, err := s.LoadEvents(context.Background(), "run-chaos-loop")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	nodeStartCount := 0
	for _, evt := range events {
		if evt.Type == store.EventNodeStarted {
			nodeStartCount++
		}
	}
	if nodeStartCount < 3 {
		t.Errorf("expected at least 3 node starts before failure, got %d", nodeStartCount)
	}
}
