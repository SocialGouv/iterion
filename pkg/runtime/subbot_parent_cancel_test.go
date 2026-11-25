package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// blockingChildExecutor parks one node (default `work`) until its context
// ends, as a CLI-delegated agent does while its process runs, and reports
// that the node is inside the call.
type blockingChildExecutor struct {
	*stubExecutor
	started chan struct{}
	node    string
}

func (b *blockingChildExecutor) Execute(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
	blocking := b.node
	if blocking == "" {
		blocking = "work"
	}
	if node.NodeID() != blocking {
		return b.stubExecutor.Execute(ctx, node, input)
	}
	close(b.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func childWorkWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "child",
		Entry: "work",
		Nodes: map[string]ir.Node{
			"work": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work"}},
			"done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "work", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
}

func parentSubbotWorkflow() *ir.Workflow {
	return &ir.Workflow{
		Name:  "parent",
		Entry: "run_child",
		Nodes: map[string]ir.Node{
			"run_child": &ir.SubbotNode{BaseNode: ir.BaseNode{ID: "run_child"}, Source: "child.bot"},
			"done":      &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges:   []*ir.Edge{{From: "run_child", To: "done"}},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
}

// TestSubbotParentCancelEndsParentCancelled: an operator cancel of a parent
// whose subbot child is mid-node ends the parent `cancelled` — not
// failed_resumable EXECUTION_FAILED, which auto-resume would re-drive — and
// the child `cancelled` too. The runner runs a real child engine under the
// context the parent hands it, as the CLI and studio runners do, so the
// error the parent reads is the one a child engine returns on cancellation.
// A drain of the parent (cause ErrRunInterrupted) keeps its own verdict,
// resumable: the cause travels with the context, not with the child's error.
func TestSubbotParentCancelEndsParentCancelled(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wf         func() *ir.Workflow
		cause      error // nil: plain operator cancel
		runner     string
		wantStatus store.RunStatus
		wantCode   store.FailureCode
	}{
		{"main loop, child engine", parentSubbotWorkflow, nil, "engine", store.RunStatusCancelled, store.FailureCancelled},
		{"main loop, child preparation", parentSubbotWorkflow, nil, "preparation", store.RunStatusCancelled, store.FailureCancelled},
		{"main loop, drain", parentSubbotWorkflow, ErrRunInterrupted, "engine", store.RunStatusFailedResumable, store.FailureInterrupted},
		{"fan-out branch, child engine", func() *ir.Workflow { return subbotFanOutWorkflow(true, 1) }, nil, "engine", store.RunStatusCancelled, store.FailureCancelled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tmpStore(t)
			childExec := &blockingChildExecutor{stubExecutor: newStubExecutor(), started: make(chan struct{})}
			const childID = "child-run"
			runner := func(ctx context.Context, req SubbotRequest) (map[string]any, error) {
				if tc.runner == "preparation" {
					// A runner stopped before the child engine exists (its
					// lease or birth waits on the parent's context): the
					// bare context error, wrapped as a runner wraps it.
					close(childExec.started)
					<-ctx.Done()
					return nil, fmt.Errorf("subbot %q: acquire lease: %w", req.Source, ctx.Err())
				}
				inv, err := prepareResourceInvocation(ctx, s, childID, req.ParentRunID)
				if err != nil {
					return nil, err
				}
				child := New(childWorkWorkflow(), s, childExec, WithParentRunID(req.ParentRunID))
				return nil, child.RunPrepared(inv.Context, childID, nil, inv)
			}
			parentExec := newStubExecutor()
			parentExec.on("entry", func(map[string]any) (map[string]any, error) {
				return map[string]any{"items": []any{map[string]any{"id": "T-1"}}}, nil
			})

			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			go func() {
				select {
				case <-childExec.started:
					cancel(tc.cause)
				case <-time.After(30 * time.Second):
				}
			}()
			eng := New(tc.wf(), s, parentExec, WithSubbotRunner(runner))
			err := eng.Run(ctx, "parent-run", nil)
			if err == nil {
				t.Fatal("parent Run returned nil after its context was cancelled")
			}

			parent, lerr := s.LoadRun(context.Background(), "parent-run")
			if lerr != nil {
				t.Fatalf("load parent: %v", lerr)
			}
			if parent.Status != tc.wantStatus || parent.FailureCode != tc.wantCode {
				t.Fatalf("parent = %s/%q (error %q), want %s/%q", parent.Status, parent.FailureCode, parent.Error, tc.wantStatus, tc.wantCode)
			}
			if tc.wantStatus == store.RunStatusCancelled && !errors.Is(err, ErrRunCancelled) {
				t.Fatalf("parent Run error = %v, want ErrRunCancelled", err)
			}
			if tc.runner != "engine" || tc.cause != nil {
				return
			}
			child, lerr := s.LoadRun(context.Background(), childID)
			if lerr != nil {
				t.Fatalf("load child: %v", lerr)
			}
			if child.Status != store.RunStatusCancelled {
				t.Fatalf("child status = %s, want cancelled", child.Status)
			}
		})
	}
}

// TestFanOutOperatorCancelEndsCancelled: the same verdict for a fan-out
// whose branch runs an ordinary agent node — the router's cancellation
// carries the context error to the run-level handler.
func TestFanOutOperatorCancelEndsCancelled(t *testing.T) {
	wf := subbotFanOutWorkflow(true, 1)
	wf.Nodes["run_child"] = &ir.AgentNode{BaseNode: ir.BaseNode{ID: "run_child"}}
	s := tmpStore(t)
	exec := &blockingChildExecutor{stubExecutor: newStubExecutor(), started: make(chan struct{}), node: "run_child"}
	exec.on("entry", func(map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{map[string]any{"id": "T-1"}}}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-exec.started:
			cancel()
		case <-time.After(30 * time.Second):
		}
	}()
	err := New(wf, s, exec).Run(ctx, "fan-run", nil)
	if !errors.Is(err, ErrRunCancelled) {
		t.Fatalf("Run error = %v, want ErrRunCancelled", err)
	}
	run, lerr := s.LoadRun(context.Background(), "fan-run")
	if lerr != nil {
		t.Fatalf("load run: %v", lerr)
	}
	if run.Status != store.RunStatusCancelled {
		t.Fatalf("run = %s/%q (error %q), want cancelled", run.Status, run.FailureCode, run.Error)
	}
}
