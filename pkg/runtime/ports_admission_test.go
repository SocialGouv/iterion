package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

func testPortsEngineConnectedOptionalWaitsWithoutDefault(t *testing.T, newStore portsTestStoreFactory) {
	source := strings.Replace(portsJoinSource, "    value: string\n", "    value: string\n      required: false\n", 1)
	source = strings.Replace(source, "    report: string\n", "    report: string\n      required: false\n", 1)
	source = strings.Replace(source, "    left: string\n    right: string\n", "    left: string\n      required: false\n      default: \"default-left\"\n    right: string\n      required: false\n      default: \"default-right\"\n", 1)
	var s store.RunStore
	executor := portsExecutorFunc(func(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
		if node.NodeID() != "c" {
			return map[string]any{}, nil
		}
		if _, present := input["left"]; present {
			return nil, errors.New("connected absent input took its unconnected default")
		}
		if _, present := input["right"]; present {
			return nil, errors.New("connected absent input took its unconnected default")
		}
		r, err := s.LoadRun(ctx, "pc1_optional")
		if err != nil {
			return nil, err
		}
		if r.PortExecution.Invocations["a"].Status != store.PortSucceeded || r.PortExecution.Invocations["b"].Status != store.PortSucceeded {
			return nil, errors.New("optional supplier was not awaited")
		}
		return map[string]any{"result": "both absent"}, nil
	})
	engine, runStore := portsTestEngine(t, newStore, source, executor)
	s = runStore
	if err := engine.Run(portsTestContext(t), "pc1_optional", map[string]any{"seed": "start"}); err != nil {
		t.Fatal(err)
	}
	if portsTestExport(t, portsTestRun(t, s, "pc1_optional"), "result") != "both absent" {
		t.Fatal("incorrect optional resolution")
	}
}

func testPortsEngineReservesRootIterationBudget(t *testing.T, newStore portsTestStoreFactory) {
	source := strings.Replace(portsMapSource, "compute render_impl:\n  expr:\n    text: \"input.prefix + input.item\"", "tool render_impl:\n  command: \"fixture\"", 1)
	source = strings.Replace(source, "  worktree: none", "  worktree: none\n  budget:\n    max_iterations: 2", 1)
	var calls atomic.Int32
	executor := portsExecutorFunc(func(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
		calls.Add(1)
		return map[string]any{"text": input["item"]}, nil
	})
	engine, s := portsTestEngine(t, newStore, source, executor)
	err := engine.Run(portsTestContext(t), "pc1_budget", map[string]any{"items": []any{"a", "b", "c", "d"}})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatal(err)
	}
	r := portsTestRun(t, s, "pc1_budget")
	if calls.Load() != 2 || r.PortExecution.Budget.Consumed.Iterations != 2 || len(r.PortExecution.Budget.Reservations) != 0 || r.PortExecution.Collections["render"].Complete {
		t.Fatal("simultaneously ready items exceeded the shared root budget")
	}
}

func testPortsEngineExactNumericInputs(t *testing.T, newStore portsTestStoreFactory) {
	source := `contract Root:
  display_name: "Exact addition"
  responsibility: "Add one to an exact integer"
  inputs:
    value: int
  outputs:
    value: int
compute add:
  expr:
    value: "input.value + 1"
workflow numeric:
  worktree: none
  runtime_semantics: "ports-v1"
  contract: Root
  graph:
    nodes:
      add:
        implementation: add
        contract: Root
    bindings:
      input.value -> add.value
    exports:
      value: add.value
`
	engine, s := portsTestEngine(t, newStore, source, newStubExecutor())
	if err := engine.Run(portsTestContext(t), "pc1_numbers", map[string]any{"value": json.Number("9007199254740993")}); err != nil {
		t.Fatal(err)
	}
	if portsTestExport(t, portsTestRun(t, s, "pc1_numbers"), "value") != json.Number("9007199254740994") {
		t.Fatal("native integer was rounded at the compute boundary")
	}
}

func portsEffectSource(recovery string) string {
	source := strings.Replace(portsMapSource, "compute render_impl:\n  expr:\n    text: \"input.prefix + input.item\"", "tool render_impl:\n  command: \"fixture\"", 1)
	source = strings.Replace(source, "contract Collect:", "  effects:\n    request:\n      description: \"External fixture request\"\n      paid: false\ncontract Collect:", 1)
	source = strings.Replace(source, "contract Render:", "  effects:\n    request:\n      description: \"External fixture request\"\n      paid: false\ncontract Render:", 1)
	source = strings.Replace(source, "  max_map_items: 4", "  max_map_items: 4\n  effects:\n    request:\n      recovery: "+recovery, 1)
	return source
}

func testPortsEngineUncertainEffectRequiresAttemptDecision(t *testing.T, newStore portsTestStoreFactory) {
	source := portsEffectSource("manual")
	var calls atomic.Int32
	executor := portsExecutorFunc(func(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("connection lost after external effect")
		}
		return map[string]any{"text": "verified replay"}, nil
	})
	engine, s := portsTestEngine(t, newStore, source, executor)
	ctx := portsTestContext(t)
	if err := engine.Run(ctx, "pc1_effect", map[string]any{"items": []any{"a"}}); err == nil {
		t.Fatal("expected external failure")
	}
	r := portsTestRun(t, s, "pc1_effect")
	if r.PortExecution.Invocations["@render[0]"].Status != store.PortUncertain {
		t.Fatal("unknown external effect labeled retryable")
	}
	resume := New(portsTestWorkflow(t, source), s, executor, WithForceResume(true), WithWorkDir(engine.workDir), WithSandboxOverride("none"))
	if err := resume.Resume(ctx, "pc1_effect", nil); !errors.Is(err, ErrPortEffectUncertain) {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("force replayed an uncertain effect")
	}
	r = portsTestRun(t, s, "pc1_effect")
	next, err := r.PortExecution.Clone()
	if err != nil {
		t.Fatal(err)
	}
	next.Revision++
	next.Invocations["@render[0]"].RecoveryDecision = "fixture operator verified request did not take effect; replay allowed"
	next.Invocations["@render[0]"].RecoveryAttempt = 1
	if err := store.SavePortExecution(ctx, s, r.ID, r.PortExecution.Revision, next); err != nil {
		t.Fatal(err)
	}
	if err := resume.Resume(ctx, "pc1_effect", nil); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || portsTestRun(t, s, "pc1_effect").PortExecution.Invocations["@render[0]"].Attempt != 2 {
		t.Fatal("attempt-bound recovery did not replay exactly the intended attempt")
	}
}

func testPortsEngineIdempotentEffectCanResume(t *testing.T, newStore portsTestStoreFactory) {
	source := portsEffectSource("idempotent")
	var calls atomic.Int32
	executor := portsExecutorFunc(func(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("transient external failure")
		}
		return map[string]any{"text": "idempotent"}, nil
	})
	engine, s := portsTestEngine(t, newStore, source, executor)
	ctx := portsTestContext(t)
	if err := engine.Run(ctx, "pc1_idempotent", map[string]any{"items": []any{"a"}}); err == nil {
		t.Fatal("expected initial failure")
	}
	resume := New(portsTestWorkflow(t, source), s, executor, WithWorkDir(engine.workDir), WithSandboxOverride("none"))
	if err := resume.Resume(ctx, "pc1_idempotent", nil); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("declared idempotent recovery did not resume")
	}
}

func testPortsEngineCrossedDAG(t *testing.T, newStore portsTestStoreFactory) {
	source := strings.Replace(portsJoinSource, "    bindings:", "      d:\n        implementation: join_impl\n        contract: Join\n      e:\n        implementation: join_impl\n        contract: Join\n      f:\n        implementation: join_impl\n        contract: Join\n    bindings:", 1)
	source = strings.Replace(source, "    exports:", "      a.value -> d.left\n      c.result -> d.right\n      b.value -> e.left\n      c.result -> e.right\n      d.result -> f.left\n      e.result -> f.right\n    exports:", 1)
	source = strings.Replace(source, "result: c.result", "result: f.result", 1)
	var calls atomic.Int32
	executor := portsExecutorFunc(func(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
		calls.Add(1)
		if node.NodeID() == "a" || node.NodeID() == "b" {
			return map[string]any{"value": node.NodeID()}, nil
		}
		return map[string]any{"result": input["left"].(string) + input["right"].(string)}, nil
	})
	engine, s := portsTestEngine(t, newStore, source, executor)
	if err := engine.Run(portsTestContext(t), "pc1_crossed", map[string]any{"seed": "start"}); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 6 || portsTestExport(t, portsTestRun(t, s, "pc1_crossed"), "result") != "aabbab" {
		t.Fatal("crossed dependency graph was flattened or duplicated")
	}
}

type portsCorrectingExecutor struct {
	portsExecutorFunc
	corrections atomic.Int32
}

func (e *portsCorrectingExecutor) CorrectOutput(ctx context.Context, node ir.Node, output map[string]any, validationErr error) (map[string]any, error) {
	e.corrections.Add(1)
	return map[string]any{"text": "repaired"}, nil
}

func testPortsEngineCorrectsProducerBeforePublication(t *testing.T, newStore portsTestStoreFactory) {
	source := strings.Replace(portsMapSource, "compute render_impl:\n  expr:\n    text: \"input.prefix + input.item\"", "tool render_impl:\n  command: \"fixture\"", 1)
	var calls atomic.Int32
	executor := &portsCorrectingExecutor{portsExecutorFunc: func(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
		calls.Add(1)
		return map[string]any{"text": false}, nil
	}}
	engine, s := portsTestEngine(t, newStore, source, executor, WithOutputCorrectionBudget(2))
	if err := engine.Run(portsTestContext(t), "pc1_correction", map[string]any{"items": []any{"a"}}); err != nil {
		t.Fatal(err)
	}
	r := portsTestRun(t, s, "pc1_correction")
	if calls.Load() != 1 || executor.corrections.Load() != 1 {
		t.Fatal("correction reran the producer or reset its bound")
	}
	if got := portsTestExport(t, r, "results"); len(got.([]any)) != 1 || got.([]any)[0] != "repaired" {
		t.Fatal(got)
	}
	for _, value := range r.PortExecution.Publications {
		if string(value.Data) == "false" {
			t.Fatal("invalid candidate was published")
		}
	}
}
