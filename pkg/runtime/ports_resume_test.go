package runtime

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

func testPortsEngineResumeReusesOnlyCommittedWork(t *testing.T, newStore portsTestStoreFactory) {
	source := strings.Replace(portsJoinSource, "input.seed -> b.seed", "a.value -> b.seed", 1)
	var mu sync.Mutex
	calls := map[string]int{}
	executor := portsExecutorFunc(func(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
		mu.Lock()
		calls[node.NodeID()]++
		count := calls[node.NodeID()]
		mu.Unlock()
		if node.NodeID() == "b" && count == 1 {
			return nil, errors.New("transient failure")
		}
		if node.NodeID() == "c" {
			return map[string]any{"result": input["left"].(string) + input["right"].(string)}, nil
		}
		return map[string]any{"value": node.NodeID()}, nil
	})
	engine, s := portsTestEngine(t, newStore, source, executor)
	ctx := portsTestContext(t)
	if err := engine.Run(ctx, "pc1_resume", map[string]any{"seed": "start"}); err == nil {
		t.Fatal("expected initial failure")
	}
	before := portsTestRun(t, s, "pc1_resume")
	resume := New(portsTestWorkflow(t, source), s, executor, WithWorkDir(engine.workDir), WithSandboxOverride("none"))
	if err := resume.Resume(ctx, "pc1_resume", nil); err != nil {
		t.Fatal(err)
	}
	after := portsTestRun(t, s, "pc1_resume")
	if !reflect.DeepEqual(before.PortExecution.Invocations["a"], after.PortExecution.Invocations["a"]) {
		t.Fatal("resume replayed or altered a committed successful producer")
	}
	if !reflect.DeepEqual(calls, map[string]int{"a": 1, "b": 2, "c": 1}) || after.PortExecution.Invocations["b"].Attempt != 2 {
		t.Fatalf("unexpected replay: %v", calls)
	}
	if after.PortExecution.Budget.Consumed.Iterations != 4 || portsTestExport(t, after, "result") != "ab" {
		t.Fatal("resume reset consumption or lost the output")
	}
}

func testPortsEngineResumeSourceChangeInvalidatesAffectedDescendants(t *testing.T, newStore portsTestStoreFactory) {
	source := strings.Replace(portsJoinSource, "tool join_impl:", "tool source_b:\n  command: \"old\"\ntool join_impl:", 1)
	source = strings.Replace(source, "      b:\n        implementation: source_impl", "      b:\n        implementation: source_b", 1)
	source = strings.Replace(source, "input.seed -> b.seed", "a.value -> b.seed", 1)
	var calls sync.Map
	executor := portsExecutorFunc(func(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
		counter, _ := calls.LoadOrStore(node.NodeID(), &atomic.Int32{})
		counter.(*atomic.Int32).Add(1)
		if node.NodeID() == "c" {
			return nil, errors.New("stop after committed a and b")
		}
		return map[string]any{"value": node.(*ir.ToolNode).Command}, nil
	})
	engine, s := portsTestEngine(t, newStore, source, executor)
	ctx := portsTestContext(t)
	if err := engine.Run(ctx, "pc1_migrate", map[string]any{"seed": "start"}); err == nil {
		t.Fatal("expected failure")
	}
	before := portsTestRun(t, s, "pc1_migrate")
	changed := strings.Replace(source, "command: \"old\"", "command: \"new\"", 1)
	resume := New(portsTestWorkflow(t, changed), s, executor, WithWorkDir(engine.workDir), WithSandboxOverride("none"))
	if err := resume.Resume(ctx, "pc1_migrate", nil); !errors.Is(err, ErrWorkflowSourceChanged) {
		t.Fatal(err)
	}
	refused := portsTestRun(t, s, "pc1_migrate")
	if !reflect.DeepEqual(before, refused) {
		t.Fatal("source refusal mutated the run before validation")
	}
	resume.forceResume = true
	if err := resume.Resume(ctx, "pc1_migrate", nil); err == nil || !strings.Contains(err.Error(), "stop after") {
		t.Fatal(err)
	}
	after := portsTestRun(t, s, "pc1_migrate")
	if !reflect.DeepEqual(before.PortExecution.Invocations["a"], after.PortExecution.Invocations["a"]) || after.PortExecution.Generation != before.PortExecution.Generation+1 {
		t.Fatal("unaffected producer was discarded or source generation did not advance")
	}
	if value := after.PortExecution.Publications[after.PortExecution.Invocations["b"].Outputs["value"]]; string(value.Data) != `"new"` {
		t.Fatal("changed implementation reused its old output")
	}
	for id, want := range map[string]int32{"a": 1, "b": 2, "c": 2} {
		got, _ := calls.Load(id)
		if got.(*atomic.Int32).Load() != want {
			t.Fatalf("%s replayed %d times", id, got.(*atomic.Int32).Load())
		}
	}
}

func testPortsEnginePauseAndResumeKeepsMappedResults(t *testing.T, newStore portsTestStoreFactory) {
	source := strings.Replace(portsMapSource, "compute render_impl:\n  expr:\n    text: \"input.prefix + input.item\"", "tool render_impl:\n  command: \"fixture\"", 1)
	source = strings.Replace(source, "  worktree: none", "  worktree: none\n  budget:\n    max_parallel_branches: 1", 1)
	pause := make(chan struct{})
	var calls atomic.Int32
	executor := portsExecutorFunc(func(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
		if calls.Add(1) == 1 {
			close(pause)
		}
		return map[string]any{"text": input["item"]}, nil
	})
	engine, s := portsTestEngine(t, newStore, source, executor, WithPauseSignal(pause))
	ctx := portsTestContext(t)
	if err := engine.Run(ctx, "pc1_pause", map[string]any{"items": []any{"a", "b", "c"}}); !errors.Is(err, ErrRunPausedOperator) {
		t.Fatal(err)
	}
	before := portsTestRun(t, s, "pc1_pause")
	if calls.Load() != 1 || before.Status != store.RunStatusPausedOperator {
		t.Fatal("pause admitted another item")
	}
	resume := New(portsTestWorkflow(t, source), s, executor, WithWorkDir(engine.workDir), WithSandboxOverride("none"))
	if err := resume.Resume(ctx, "pc1_pause", nil); err != nil {
		t.Fatal(err)
	}
	after := portsTestRun(t, s, "pc1_pause")
	if calls.Load() != 3 || !reflect.DeepEqual(before.PortExecution.Invocations["@render[0]"], after.PortExecution.Invocations["@render[0]"]) {
		t.Fatal("resume replayed a committed map item")
	}
	if got := portsTestExport(t, after, "results"); !reflect.DeepEqual(got, []any{"a", "b", "c"}) {
		t.Fatal(got)
	}
}

func testPortsEngineOperatorCancelWinsAgainstFinalization(t *testing.T, newStore portsTestStoreFactory) {
	var s store.RunStore
	executor := portsExecutorFunc(func(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
		if node.NodeID() == "c" {
			if err := s.UpdateRunStatus(ctx, "pc1_cancel", store.RunStatusCancelled, "operator"); err != nil {
				return nil, err
			}
			return map[string]any{"result": "complete"}, nil
		}
		return map[string]any{"value": node.NodeID()}, nil
	})
	engine, runStore := portsTestEngine(t, newStore, portsJoinSource, executor)
	s = runStore
	if err := engine.Run(portsTestContext(t), "pc1_cancel", map[string]any{"seed": "start"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if portsTestRun(t, s, "pc1_cancel").Status != store.RunStatusCancelled {
		t.Fatal("completion overwrote cancellation")
	}
}

func testPortsEngineCancelPreventsFurtherAdmissionWithoutContextSignal(t *testing.T, newStore portsTestStoreFactory) {
	source := strings.Replace(portsJoinSource, "input.seed -> b.seed", "a.value -> b.seed", 1)
	var s store.RunStore
	var calls atomic.Int32
	executor := portsExecutorFunc(func(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
		calls.Add(1)
		if err := s.UpdateRunStatus(ctx, "pc1_cancel_admission", store.RunStatusCancelled, "operator"); err != nil {
			return nil, err
		}
		return map[string]any{"value": "a"}, nil
	})
	engine, runStore := portsTestEngine(t, newStore, source, executor)
	s = runStore
	if err := engine.Run(portsTestContext(t), "pc1_cancel_admission", map[string]any{"seed": "start"}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if calls.Load() != 1 || portsTestRun(t, s, "pc1_cancel_admission").PortExecution.Invocations["b"].Status != store.PortPending {
		t.Fatal("canceled root admitted another invocation")
	}
}

func testPortsEngineResumeCannotChangeInterpreterWithForce(t *testing.T, newStore portsTestStoreFactory) {
	engine, s := portsTestEngine(t, newStore, portsMapSource, newStubExecutor())
	ctx := portsTestContext(t)
	if err := engine.Run(ctx, "pc1_semantics", map[string]any{"items": []any{"a", "b", "c", "d", "e"}}); err == nil {
		t.Fatal("expected cardinality failure")
	}
	before := portsTestRun(t, s, "pc1_semantics")
	legacy := portsTestWorkflow(t, "compute first:\n  expr:\n    value: \"1\"\nworkflow legacy:\n  entry: first\n  first -> done\n")
	resume := New(legacy, s, newStubExecutor(), WithForceResume(true), WithWorkDir(engine.workDir))
	if err := resume.Resume(ctx, "pc1_semantics", nil); !errors.Is(err, store.ErrRunSemantics) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, portsTestRun(t, s, "pc1_semantics")) {
		t.Fatal("forced interpreter mismatch mutated native state")
	}
}
