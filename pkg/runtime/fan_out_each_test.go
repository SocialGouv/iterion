package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// ===========================================================================
// fan_out_each — data-driven fan-out + dependency-DAG scheduling tests
// ===========================================================================

// item builds a per-element map: {id, deps}.
func item(id string, deps ...string) map[string]interface{} {
	d := make([]interface{}, len(deps))
	for i, x := range deps {
		d[i] = x
	}
	return map[string]interface{}{"id": id, "deps": d}
}

// fanOutEachWorkflow builds:
//
//	entry(source) -> dispatch(fan_out_each over entry.items) -> handle -> collect(wait_all) -> done
//
// When dag is true the router carries key="id" / depends_on="deps". The
// handle node receives its item's id via the dispatch->handle with-mapping.
func fanOutEachWorkflow(dag bool, await ir.AwaitMode, maxParallel int) *ir.Workflow {
	router := &ir.RouterNode{
		BaseNode:    ir.BaseNode{ID: "dispatch"},
		RouterMode:  ir.RouterFanOutEach,
		Over:        "{{outputs.entry.items}}",
		OverRefs:    []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"entry", "items"}, Raw: "{{outputs.entry.items}}"}},
		ItemBinding: "item",
	}
	if dag {
		router.KeyField = "id"
		router.DepsField = "deps"
	}
	wf := &ir.Workflow{
		Name:  "fan_out_each_test",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"dispatch": router,
			"handle":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "handle"}},
			"collect":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "collect"}, AwaitMode: await},
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":     &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "dispatch"},
			{From: "dispatch", To: "handle", With: []*ir.DataMapping{
				{Key: "id", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"dispatch", "item", "id"}, Raw: "{{outputs.dispatch.item.id}}"}}, Raw: "{{outputs.dispatch.item.id}}"},
			}},
			{From: "handle", To: "collect"},
			{From: "collect", To: "done"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
	if maxParallel > 0 {
		wf.Budget = &ir.Budget{MaxParallelBranches: maxParallel}
	}
	return wf
}

// ---------------------------------------------------------------------------
// buildFanOutDAG — pure dependency-graph builder + validation
// ---------------------------------------------------------------------------

func TestBuildFanOutDAG_ValidDiamond(t *testing.T) {
	items := []interface{}{item("A"), item("B", "A"), item("C", "A"), item("D", "B", "C")}
	deps, err := buildFanOutDAG(items, "id", "deps")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// indices: A=0 B=1 C=2 D=3
	want := [][]int{{}, {0}, {0}, {1, 2}}
	if len(deps) != 4 {
		t.Fatalf("expected 4 dep lists, got %d", len(deps))
	}
	for i := range want {
		if len(deps[i]) != len(want[i]) {
			t.Errorf("deps[%d] = %v, want %v", i, deps[i], want[i])
			continue
		}
		for j := range want[i] {
			if deps[i][j] != want[i][j] {
				t.Errorf("deps[%d] = %v, want %v", i, deps[i], want[i])
			}
		}
	}
}

func TestBuildFanOutDAG_EmptyDepsAllParallel(t *testing.T) {
	items := []interface{}{item("A"), item("B"), item("C")}
	deps, err := buildFanOutDAG(items, "id", "deps")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i, d := range deps {
		if len(d) != 0 {
			t.Errorf("deps[%d] = %v, want empty", i, d)
		}
	}
}

func TestBuildFanOutDAG_Cycle(t *testing.T) {
	items := []interface{}{item("A", "B"), item("B", "A")}
	_, err := buildFanOutDAG(items, "id", "deps")
	if err == nil || !contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

func TestBuildFanOutDAG_SelfCycle(t *testing.T) {
	items := []interface{}{item("A", "A")}
	_, err := buildFanOutDAG(items, "id", "deps")
	if err == nil || !contains(err.Error(), "itself") {
		t.Fatalf("expected self-dependency error, got %v", err)
	}
}

func TestBuildFanOutDAG_UnknownDep(t *testing.T) {
	items := []interface{}{item("A", "Z")}
	_, err := buildFanOutDAG(items, "id", "deps")
	if err == nil || !contains(err.Error(), "unknown id") {
		t.Fatalf("expected unknown-id error, got %v", err)
	}
}

func TestBuildFanOutDAG_DuplicateKey(t *testing.T) {
	items := []interface{}{item("A"), item("A")}
	_, err := buildFanOutDAG(items, "id", "deps")
	if err == nil || !contains(err.Error(), "duplicate key") {
		t.Fatalf("expected duplicate-key error, got %v", err)
	}
}

func TestBuildFanOutDAG_MissingKey(t *testing.T) {
	items := []interface{}{map[string]interface{}{"deps": []interface{}{}}}
	_, err := buildFanOutDAG(items, "id", "deps")
	if err == nil || !contains(err.Error(), "missing key") {
		t.Fatalf("expected missing-key error, got %v", err)
	}
}

func TestBuildFanOutDAG_NonObjectItem(t *testing.T) {
	items := []interface{}{"not-an-object"}
	_, err := buildFanOutDAG(items, "id", "deps")
	if err == nil || !contains(err.Error(), "not an object") {
		t.Fatalf("expected non-object error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// coerceToArray — array-source coercion
// ---------------------------------------------------------------------------

func TestCoerceToArray(t *testing.T) {
	// native array
	if a, err := coerceToArray([]interface{}{1, 2}, "r", "o"); err != nil || len(a) != 2 {
		t.Errorf("native array: got %v, %v", a, err)
	}
	// JSON string holding an array (the DSL json-field path)
	if a, err := coerceToArray(`[{"id":"A"},{"id":"B"}]`, "r", "o"); err != nil || len(a) != 2 {
		t.Errorf("json string: got %v, %v", a, err)
	}
	// empty string -> empty array
	if a, err := coerceToArray("   ", "r", "o"); err != nil || len(a) != 0 {
		t.Errorf("empty string: got %v, %v", a, err)
	}
	// nil -> error
	if _, err := coerceToArray(nil, "r", "o"); err == nil {
		t.Error("nil should error")
	}
	// non-array scalar -> error
	if _, err := coerceToArray(42, "r", "o"); err == nil {
		t.Error("scalar should error")
	}
	// non-array JSON string -> error
	if _, err := coerceToArray(`{"x":1}`, "r", "o"); err == nil {
		t.Error("json object string should error")
	}
}

// ---------------------------------------------------------------------------
// execFanOutEach — end-to-end via the engine + stub executor
// ---------------------------------------------------------------------------

// TestFanOutEach_AllParallel: no deps → every item runs, each handle gets its
// own per-item id, the run converges and finishes.
func TestFanOutEach_AllParallel(t *testing.T) {
	wf := fanOutEachWorkflow(false, ir.AwaitWaitAll, 0)

	var mu sync.Mutex
	seen := map[string]bool{}
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"items": []interface{}{item("A"), item("B"), item("C")}}, nil
	})
	exec.on("handle", func(input map[string]interface{}) (map[string]interface{}, error) {
		mu.Lock()
		seen[input["id"].(string)] = true
		mu.Unlock()
		return map[string]interface{}{"ok": true}, nil
	})
	exec.on("collect", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"done": true}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-fe-parallel", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r, _ := s.LoadRun(context.Background(), "run-fe-parallel")
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected finished, got %s", r.Status)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, id := range []string{"A", "B", "C"} {
		if !seen[id] {
			t.Errorf("item %s was never dispatched to handle (per-item binding broken)", id)
		}
	}
}

// TestFanOutEach_EmptyArray: 0 items → skip straight to convergence, finish.
func TestFanOutEach_EmptyArray(t *testing.T) {
	wf := fanOutEachWorkflow(false, ir.AwaitWaitAll, 0)
	var handleCalls int64
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"items": []interface{}{}}, nil
	})
	exec.on("handle", func(_ map[string]interface{}) (map[string]interface{}, error) {
		atomic.AddInt64(&handleCalls, 1)
		return map[string]interface{}{}, nil
	})
	exec.on("collect", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})
	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-fe-empty", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if atomic.LoadInt64(&handleCalls) != 0 {
		t.Errorf("handle ran %d times on an empty array, want 0", handleCalls)
	}
	r, _ := s.LoadRun(context.Background(), "run-fe-empty")
	if r.Status != store.RunStatusFinished {
		t.Errorf("expected finished, got %s", r.Status)
	}
}

// TestFanOutEach_DAG_DiamondOrderingAndParallelism: A -> {B,C} -> D.
// Asserts (1) dependency: A finishes before B/C start, and B/C finish before
// D starts; (2) parallelism: B and C overlap (proven by a 2-way barrier — a
// serial scheduler would deadlock the barrier and trip the timeout).
func TestFanOutEach_DAG_DiamondOrderingAndParallelism(t *testing.T) {
	wf := fanOutEachWorkflow(true, ir.AwaitWaitAll, 4)

	var mu sync.Mutex
	var order []string
	record := func(s string) { mu.Lock(); order = append(order, s); mu.Unlock() }

	// 2-way barrier for the independent siblings B and C.
	var arrived int64
	release := make(chan struct{})

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"items": []interface{}{
			item("A"), item("B", "A"), item("C", "A"), item("D", "B", "C"),
		}}, nil
	})
	exec.on("handle", func(input map[string]interface{}) (map[string]interface{}, error) {
		id := input["id"].(string)
		record(id + ":start")
		if id == "B" || id == "C" {
			// Force B and C to overlap: each arrives, the 2nd releases both.
			if atomic.AddInt64(&arrived, 1) == 2 {
				close(release)
			}
			select {
			case <-release:
			case <-time.After(2 * time.Second):
				t.Errorf("siblings B/C did not run concurrently (barrier timed out)")
			}
		}
		record(id + ":end")
		return map[string]interface{}{"ok": true}, nil
	})
	exec.on("collect", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-fe-diamond", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	r, _ := s.LoadRun(context.Background(), "run-fe-diamond")
	if r.Status != store.RunStatusFinished {
		t.Fatalf("expected finished, got %s", r.Status)
	}

	mu.Lock()
	defer mu.Unlock()
	idx := func(s string) int {
		for i, v := range order {
			if v == s {
				return i
			}
		}
		return -1
	}
	// Dependency: A ends before B and C start.
	if !(idx("A:end") < idx("B:start") && idx("A:end") < idx("C:start")) {
		t.Errorf("A must finish before B/C start; order=%v", order)
	}
	// Dependency: D starts only after B and C end.
	if !(idx("D:start") > idx("B:end") && idx("D:start") > idx("C:end")) {
		t.Errorf("D must start after B/C end; order=%v", order)
	}
}

// TestFanOutEach_DAG_BoundedParallelism: 4 independent items, cap 2 →
// at most 2 run concurrently.
func TestFanOutEach_DAG_BoundedParallelism(t *testing.T) {
	wf := fanOutEachWorkflow(true, ir.AwaitWaitAll, 2)
	var maxC, curC int64
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"items": []interface{}{item("A"), item("B"), item("C"), item("D")}}, nil
	})
	exec.on("handle", func(_ map[string]interface{}) (map[string]interface{}, error) {
		cur := atomic.AddInt64(&curC, 1)
		for {
			old := atomic.LoadInt64(&maxC)
			if cur <= old || atomic.CompareAndSwapInt64(&maxC, old, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt64(&curC, -1)
		return map[string]interface{}{}, nil
	})
	exec.on("collect", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})
	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-fe-bounded", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if m := atomic.LoadInt64(&maxC); m > 2 {
		t.Errorf("max concurrent = %d, want <= 2 (budget cap)", m)
	}
}

// TestFanOutEach_DAG_FailedDepSkipsDependents: A fails → B (deps A) is skipped
// (its handle never runs) and the run fails under wait_all.
func TestFanOutEach_DAG_FailedDepSkipsDependents(t *testing.T) {
	wf := fanOutEachWorkflow(true, ir.AwaitWaitAll, 4)
	var bRan int64
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"items": []interface{}{item("A"), item("B", "A")}}, nil
	})
	exec.on("handle", func(input map[string]interface{}) (map[string]interface{}, error) {
		id := input["id"].(string)
		if id == "A" {
			return nil, errors.New("A boom")
		}
		atomic.AddInt64(&bRan, 1) // B must never get here
		return map[string]interface{}{}, nil
	})
	exec.on("collect", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})
	s := tmpStore(t)
	eng := New(wf, s, exec)
	err := eng.Run(context.Background(), "run-fe-skip", nil)
	if err == nil {
		t.Fatal("expected run failure (A failed, wait_all)")
	}
	if atomic.LoadInt64(&bRan) != 0 {
		t.Errorf("B ran despite its dependency A failing — dependents must be skipped")
	}
}

// TestFanOutEach_DAG_CycleFailsRun: a runtime dependency cycle fails the run
// up-front with a cycle diagnostic (no branch executes).
func TestFanOutEach_DAG_CycleFailsRun(t *testing.T) {
	wf := fanOutEachWorkflow(true, ir.AwaitWaitAll, 4)
	var handleRan int64
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{"items": []interface{}{item("A", "B"), item("B", "A")}}, nil
	})
	exec.on("handle", func(_ map[string]interface{}) (map[string]interface{}, error) {
		atomic.AddInt64(&handleRan, 1)
		return map[string]interface{}{}, nil
	})
	exec.on("collect", func(_ map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{}, nil
	})
	s := tmpStore(t)
	eng := New(wf, s, exec)
	err := eng.Run(context.Background(), "run-fe-cycle", nil)
	if err == nil || !contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle failure, got %v", err)
	}
	if atomic.LoadInt64(&handleRan) != 0 {
		t.Errorf("handle ran %d times on a cyclic DAG, want 0 (reject before scheduling)", handleRan)
	}
}

// contains is a tiny substring helper (avoids importing strings just for tests).
func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
