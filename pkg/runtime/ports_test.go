package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/internal/gittest"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/portsactivation"
	"github.com/SocialGouv/iterion/pkg/store"
)

// These tests use the real parser, compiler, Engine and atomic filesystem
// RunStore. Only external execution is replaced, so a compiler-only graph or
// an executor replay cannot satisfy the assertions.
const portsMapSource = `dsl: 2
contract Batch:
  display_name: "Render a batch"
  responsibility: "Deliver rendered values"
  inputs:
    items: string[]
    prefix: string
      required: false
      default: "item-"
  outputs:
    results: string[]
contract Render:
  display_name: "Render an item"
  responsibility: "Prefix one item"
  inputs:
    item: string
    prefix: string
  outputs:
    text: string
contract Collect:
  display_name: "Collect results"
  responsibility: "Receive the complete batch"
  inputs:
    texts: string[]
  outputs:
    texts: string[]
port_policy limited:
  max_map_items: 4
compute render_impl:
  expr:
    text: "input.prefix + input.item"
compute collect_impl:
  expr:
    texts: "input.texts"
workflow batch:
  worktree: none
  runtime_semantics: "ports-v1"
  contract: Batch
  port_policy: limited
  graph:
    nodes:
      render:
        implementation: render_impl
        contract: Render
      collect:
        implementation: collect_impl
        contract: Collect
    bindings:
      input.items -> render.item
      input.prefix -> render.prefix
      render.text -> collect.texts
    exports:
      results: collect.texts
    products: ["results"]
`

const portsJoinSource = `dsl: 2
contract Root:
  display_name: "Join two producers"
  responsibility: "Deliver both results"
  inputs:
    seed: string
  outputs:
    result: string
    report: string
contract Producer:
  display_name: "Produce a value"
  responsibility: "Produce one value"
  inputs:
    seed: string
  outputs:
    value: string
contract Join:
  display_name: "Combine values"
  responsibility: "Wait for both producers"
  inputs:
    left: string
    right: string
  outputs:
    result: string
tool source_impl:
  command: "fixture"
tool join_impl:
  command: "fixture"
workflow join:
  worktree: none
  runtime_semantics: "ports-v1"
  contract: Root
  graph:
    nodes:
      a:
        implementation: source_impl
        contract: Producer
      b:
        implementation: source_impl
        contract: Producer
      c:
        implementation: join_impl
        contract: Join
    bindings:
      input.seed -> a.seed
      input.seed -> b.seed
      a.value -> c.left
      b.value -> c.right
    exports:
      result: c.result
      report: a.value
    products: ["report", "result"]
`

type portsExecutorFunc func(context.Context, ir.Node, map[string]any) (map[string]any, error)

func (f portsExecutorFunc) Execute(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
	return f(ctx, node, input)
}

func portsTestWorkflow(t testing.TB, source string) *ir.Workflow {
	t.Helper()
	parsed := parser.Parse("ports.bot", source)
	if len(parsed.Diagnostics) != 0 {
		t.Fatalf("parse: %v", parsed.Diagnostics)
	}
	compiled := ir.Compile(parsed.File)
	if compiled.HasErrors() {
		t.Fatalf("compile: %v", compiled.Diagnostics)
	}
	return compiled.Workflow
}

func portsTestEngine(t *testing.T, newStore portsTestStoreFactory, source string, executor NodeExecutor, opts ...EngineOption) (*Engine, store.RunStore) {
	t.Helper()
	s := newStore(t)
	activatePortsTestStore(t, s)
	options := append([]EngineOption{WithWorkDir(gittest.SourceRepo(t)), WithSandboxOverride("none")}, opts...)
	return New(portsTestWorkflow(t, source), s, executor, options...), s
}

func activatePortsTestStore(t *testing.T, s store.RunStore) {
	t.Helper()
	identity, err := store.PortStoreIdentity(s)
	if err != nil {
		t.Fatal(err)
	}
	scope := store.PortActivationLocal
	record := &store.PortActivation{Version: store.PortActivationVersion, Revision: 1, Enabled: true, Scope: scope, StoreIdentity: identity,
		ProofDigest: strings.Repeat("a", 64), VerifiedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour)}
	if s.Root() == "" {
		record.Scope, record.QueueVersion, record.ConsumerAccessEvidence = store.PortActivationDistributed, 15, "isolated disposable test consumer"
	}
	record.CapabilityDigest = portsactivation.CapabilityDigest(record.Scope)
	if err := store.AsPortActivationStore(s).SavePortActivation(context.Background(), 0, record); err != nil {
		t.Fatal(err)
	}
}

func portsTestContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	return store.WithIdentity(ctx, "ports-fixture", "owner")
}

func portsTestRun(t *testing.T, s store.RunStore, id string) *store.Run {
	t.Helper()
	r, err := s.LoadRun(store.WithIdentity(context.Background(), "ports-fixture", "owner"), id)
	if err != nil {
		t.Fatal(err)
	}
	if r.PortExecution == nil {
		t.Fatal("no durable native execution")
	}
	return r
}

func portsTestExport(t *testing.T, r *store.Run, name string) any {
	t.Helper()
	value := r.PortExecution.Publications[r.PortExecution.Exports[name]]
	if value == nil {
		t.Fatalf("missing public export %s", name)
	}
	decoded, err := ir.DecodePortValue(value.Data)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

func testPortsEngineMapZeroOneMany(t *testing.T, newStore portsTestStoreFactory) {
	for _, count := range []int{0, 1, 4} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			items, want := make([]any, count), make([]any, count)
			for i := range count {
				items[i], want[i] = fmt.Sprint(i), "item-"+fmt.Sprint(i)
			}
			engine, s := portsTestEngine(t, newStore, portsMapSource, portsExecutorFunc(func(context.Context, ir.Node, map[string]any) (map[string]any, error) {
				return nil, errors.New("compute unexpectedly dispatched externally")
			}))
			if err := engine.Run(portsTestContext(t), "pc1_map", map[string]any{"items": items}); err != nil {
				t.Fatal(err)
			}
			r := portsTestRun(t, s, "pc1_map")
			if r.Status != store.RunStatusFinished || r.Checkpoint != nil || r.RuntimeSemantics != ir.RuntimeSemanticsPortsV1 {
				t.Fatalf("native execution used legacy checkpoint/control: %s", r.Status)
			}
			if got := portsTestExport(t, r, "results"); !reflect.DeepEqual(got, want) {
				t.Fatalf("collected %v, want %v", got, want)
			}
			if len(r.PortExecution.Invocations) != count+1 || r.PortExecution.Budget.Consumed.Iterations != int64(count+1) {
				t.Fatalf("wrong actual invocation count: %+v", r.PortExecution.Budget)
			}
			if !reflect.DeepEqual(r.PortExecution.Products, []string{"results"}) {
				t.Fatal("explicit product designation lost")
			}
		})
	}
}

func testPortsEngineMapCollectsInInputOrder(t *testing.T, newStore portsTestStoreFactory) {
	source := strings.Replace(portsMapSource, "compute render_impl:\n  expr:\n    text: \"input.prefix + input.item\"", "tool render_impl:\n  command: \"fixture\"", 1)
	started := make(chan string, 4)
	finish := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{}), "c": make(chan struct{})}
	returned := make(chan string, 4)
	executor := portsExecutorFunc(func(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
		item := input["item"].(string)
		started <- item
		select {
		case <-finish[item]:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		returned <- item
		return map[string]any{"text": input["prefix"].(string) + item}, nil
	})
	engine, s := portsTestEngine(t, newStore, source, executor)
	ctx := portsTestContext(t)
	done := make(chan error, 1)
	go func() { done <- engine.Run(ctx, "pc1_order", map[string]any{"items": []any{"a", "b", "c"}}) }()
	for range 3 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("map items were not admitted concurrently")
		}
	}
	for _, item := range []string{"c", "b", "a"} {
		close(finish[item])
		select {
		case got := <-returned:
			if got != item {
				t.Fatalf("return order %s, want %s", got, item)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := portsTestExport(t, portsTestRun(t, s, "pc1_order"), "results"); !reflect.DeepEqual(got, []any{"item-a", "item-b", "item-c"}) {
		t.Fatal(got)
	}
}

func testPortsEngineJoinWaitsForCommittedProducers(t *testing.T, newStore portsTestStoreFactory) {
	started := make(chan string, 2)
	release := make(chan struct{})
	var s store.RunStore
	executor := portsExecutorFunc(func(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
		if node.NodeID() != "c" {
			started <- node.NodeID()
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return map[string]any{"value": node.NodeID()}, nil
		}
		r, err := s.LoadRun(ctx, "pc1_join")
		if err != nil {
			return nil, err
		}
		for _, producer := range []string{"a", "b"} {
			invocation := r.PortExecution.Invocations[producer]
			if invocation.Status != store.PortSucceeded || r.PortExecution.Publications[invocation.Outputs["value"]] == nil {
				return nil, fmt.Errorf("consumer ran before %s was committed", producer)
			}
		}
		return map[string]any{"result": input["left"].(string) + input["right"].(string)}, nil
	})
	engine, runStore := portsTestEngine(t, newStore, portsJoinSource, executor)
	s = runStore
	ctx := portsTestContext(t)
	done := make(chan error, 1)
	go func() { done <- engine.Run(ctx, "pc1_join", map[string]any{"seed": "start"}) }()
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("independent producers were serialized")
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	r := portsTestRun(t, s, "pc1_join")
	if portsTestExport(t, r, "result") != "ab" || portsTestExport(t, r, "report") != "a" {
		t.Fatal("consumed and product export lost their shared source")
	}
}

func testPortsEngineRejectsMapLimitBeforeCreatingItems(t *testing.T, newStore portsTestStoreFactory) {
	engine, s := portsTestEngine(t, newStore, portsMapSource, newStubExecutor())
	err := engine.Run(portsTestContext(t), "pc1_limit", map[string]any{"items": []any{"a", "b", "c", "d", "e"}})
	if err == nil || !strings.Contains(err.Error(), "max_map_items") {
		t.Fatal(err)
	}
	r := portsTestRun(t, s, "pc1_limit")
	if r.Status != store.RunStatusFailedResumable || len(r.PortExecution.Collections) != 0 || len(r.PortExecution.Invocations) != 1 {
		t.Fatal("map allocated item records before enforcing its cardinality limit")
	}
}

func testPortsEngineStrictFailureCancelsSiblingsAndStopsAdmission(t *testing.T, newStore portsTestStoreFactory) {
	source := strings.Replace(portsMapSource, "compute render_impl:\n  expr:\n    text: \"input.prefix + input.item\"", "tool render_impl:\n  command: \"fixture\"", 1)
	source = strings.Replace(source, "  worktree: none", "  worktree: none\n  budget:\n    max_parallel_branches: 2", 1)
	var calls atomic.Int32
	var once sync.Once
	siblingStarted := make(chan struct{})
	executor := portsExecutorFunc(func(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
		calls.Add(1)
		if input["item"] == "a" {
			select {
			case <-siblingStarted:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return nil, errors.New("producer failed")
		}
		once.Do(func() { close(siblingStarted) })
		<-ctx.Done()
		return nil, ctx.Err()
	})
	engine, s := portsTestEngine(t, newStore, source, executor)
	err := engine.Run(portsTestContext(t), "pc1_failure", map[string]any{"items": []any{"a", "b", "c", "d"}})
	if err == nil || !strings.Contains(err.Error(), "producer failed") {
		t.Fatal(err)
	}
	r := portsTestRun(t, s, "pc1_failure")
	if calls.Load() != 2 || r.PortExecution.Collections["render"].Complete || r.PortExecution.Invocations["collect"].Status != store.PortPending {
		t.Fatalf("strict failure admitted more work or published partial collection: calls=%d", calls.Load())
	}
	if r.PortExecution.Invocations["@render[1]"].Status != store.PortCanceled || len(r.PortExecution.Budget.Reservations) != 0 {
		t.Fatal("canceled sibling or reservation was left active")
	}
}

func testPortsEngineInvalidOutputBlocksConsumer(t *testing.T, newStore portsTestStoreFactory) {
	var joined atomic.Bool
	executor := portsExecutorFunc(func(ctx context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
		if node.NodeID() == "c" {
			joined.Store(true)
		}
		return map[string]any{}, nil
	})
	engine, s := portsTestEngine(t, newStore, portsJoinSource, executor)
	err := engine.Run(portsTestContext(t), "pc1_invalid", map[string]any{"seed": "start"})
	if err == nil {
		t.Fatal("missing mandatory output was accepted")
	}
	r := portsTestRun(t, s, "pc1_invalid")
	if joined.Load() || len(r.PortExecution.Exports) != 0 || r.Status == store.RunStatusFinished {
		t.Fatal("invalid output unblocked a consumer or a required product")
	}
}
