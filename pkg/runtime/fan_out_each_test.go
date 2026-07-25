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
func item(id string, deps ...string) map[string]any {
	d := make([]any, len(deps))
	for i, x := range deps {
		d[i] = x
	}
	return map[string]any{"id": id, "deps": d}
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
	items := []any{item("A"), item("B", "A"), item("C", "A"), item("D", "B", "C")}
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
	items := []any{item("A"), item("B"), item("C")}
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
	items := []any{item("A", "B"), item("B", "A")}
	_, err := buildFanOutDAG(items, "id", "deps")
	if err == nil || !contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

func TestBuildFanOutDAG_SelfCycle(t *testing.T) {
	items := []any{item("A", "A")}
	_, err := buildFanOutDAG(items, "id", "deps")
	if err == nil || !contains(err.Error(), "itself") {
		t.Fatalf("expected self-dependency error, got %v", err)
	}
}

func TestBuildFanOutDAG_UnknownDep(t *testing.T) {
	items := []any{item("A", "Z")}
	_, err := buildFanOutDAG(items, "id", "deps")
	if err == nil || !contains(err.Error(), "unknown id") {
		t.Fatalf("expected unknown-id error, got %v", err)
	}
}

func TestBuildFanOutDAG_DuplicateKey(t *testing.T) {
	items := []any{item("A"), item("A")}
	_, err := buildFanOutDAG(items, "id", "deps")
	if err == nil || !contains(err.Error(), "duplicate key") {
		t.Fatalf("expected duplicate-key error, got %v", err)
	}
}

func TestBuildFanOutDAG_MissingKey(t *testing.T) {
	items := []any{map[string]any{"deps": []any{}}}
	_, err := buildFanOutDAG(items, "id", "deps")
	if err == nil || !contains(err.Error(), "missing key") {
		t.Fatalf("expected missing-key error, got %v", err)
	}
}

func TestBuildFanOutDAG_NonObjectItem(t *testing.T) {
	items := []any{"not-an-object"}
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
	if a, err := coerceToArray([]any{1, 2}, "r", "o"); err != nil || len(a) != 2 {
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
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{item("A"), item("B"), item("C")}}, nil
	})
	exec.on("handle", func(input map[string]any) (map[string]any, error) {
		mu.Lock()
		seen[input["id"].(string)] = true
		mu.Unlock()
		return map[string]any{"ok": true}, nil
	})
	exec.on("collect", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"done": true}, nil
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
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{}}, nil
	})
	exec.on("handle", func(_ map[string]any) (map[string]any, error) {
		atomic.AddInt64(&handleCalls, 1)
		return map[string]any{}, nil
	})
	exec.on("collect", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
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

// TestResourceSemaphore_BoundsConcurrency: a fan_out_each over N items whose
// branch node declares `needs: slot` never runs more than the resource's
// capacity at once — even with NO max_parallel_branches (all N branches are
// eligible immediately). The capacity==N control proves the semaphore (not
// something else) is the lever: peak rises to N.
func TestResourceSemaphore_BoundsConcurrency(t *testing.T) {
	runPeak := func(runID string, capacity, nItems int) int32 {
		wf := fanOutEachWorkflow(false, ir.AwaitWaitAll, 0) // 0 = no max_parallel cap
		wf.Resources = map[string]int{"slot": capacity}
		wf.Nodes["handle"].(*ir.AgentNode).Needs = []string{"slot"}

		items := make([]any, nItems)
		for i := range items {
			items[i] = item(string(rune('A' + i)))
		}

		// expectedPeak holders barrier on each other before any releases, so the
		// measured peak is deterministic (no reliance on sleep/scheduler timing):
		// the semaphore caps concurrency at min(capacity, nItems), and the
		// barrier forces exactly that many to overlap.
		expectedPeak := capacity
		if nItems < expectedPeak {
			expectedPeak = nItems
		}
		var active, peak int32
		barrier := make(chan struct{})
		var once sync.Once
		exec := newStubExecutor()
		exec.on("entry", func(_ map[string]any) (map[string]any, error) {
			return map[string]any{"items": items}, nil
		})
		exec.on("handle", func(_ map[string]any) (map[string]any, error) {
			n := atomic.AddInt32(&active, 1)
			for { // lock-free max
				p := atomic.LoadInt32(&peak)
				if n <= p || atomic.CompareAndSwapInt32(&peak, p, n) {
					break
				}
			}
			if n >= int32(expectedPeak) {
				once.Do(func() { close(barrier) })
			}
			<-barrier // block until expectedPeak holders are concurrently inside
			atomic.AddInt32(&active, -1)
			return map[string]any{"ok": true}, nil
		})
		exec.on("collect", func(_ map[string]any) (map[string]any, error) {
			return map[string]any{}, nil
		})

		s := tmpStore(t)
		eng := New(wf, s, exec)
		if err := eng.Run(context.Background(), runID, nil); err != nil {
			t.Fatalf("run error: %v", err)
		}
		r, _ := s.LoadRun(context.Background(), runID)
		if r.Status != store.RunStatusFinished {
			t.Fatalf("expected finished, got %s", r.Status)
		}
		return atomic.LoadInt32(&peak)
	}

	if got := runPeak("run-res-cap2", 2, 6); got != 2 {
		t.Errorf("slot capacity 2: peak concurrency = %d, want 2 (semaphore did not bound)", got)
	}
	if got := runPeak("run-res-cap6", 6, 6); got != 6 {
		t.Errorf("slot capacity 6 control: peak concurrency = %d, want 6 (capacity is not the lever)", got)
	}
}

// TestResourceLease_DistinctInstances proves the lease form of a named resource
// (`godot: [godot-s1, godot-s2, godot-s3]`) hands every concurrently-running
// node a DISTINCT instance id (surfaced under input["_lease"]), never sharing an
// id across overlapping holders, never exceeding the pool size, and only ever
// leasing ids from the declared pool. This is what makes a counting bound become
// a real pool of distinct Godot editors.
func TestResourceLease_DistinctInstances(t *testing.T) {
	pool := []string{"godot-s1", "godot-s2", "godot-s3"}

	wf := fanOutEachWorkflow(false, ir.AwaitWaitAll, 0) // 0 = no max_parallel cap
	wf.Resources = map[string]int{"godot": len(pool)}
	wf.ResourceMembers = map[string][]string{"godot": pool}
	wf.Nodes["handle"].(*ir.AgentNode).Needs = []string{"godot"}

	const nItems = 8
	items := make([]any, nItems)
	for i := range items {
		items[i] = item(string(rune('A' + i)))
	}

	var mu sync.Mutex
	held := map[string]bool{} // currently-leased ids
	seen := map[string]bool{} // every id ever leased
	maxConcurrent := 0
	var violations []string

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"items": items}, nil
	})
	exec.on("handle", func(in map[string]any) (map[string]any, error) {
		lease, _ := in[leaseInputKey].(map[string]string)
		id := lease["godot"]

		mu.Lock()
		switch {
		case id == "":
			violations = append(violations, "handle ran with no godot lease")
		case held[id]:
			violations = append(violations, "id "+id+" leased to two concurrent holders")
		default:
			held[id] = true
			seen[id] = true
			if len(held) > maxConcurrent {
				maxConcurrent = len(held)
			}
		}
		mu.Unlock()

		time.Sleep(15 * time.Millisecond) // hold the lease so contention is observable

		mu.Lock()
		delete(held, id)
		mu.Unlock()
		return map[string]any{"ok": true}, nil
	})
	exec.on("collect", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-lease", nil); err != nil {
		t.Fatalf("run error: %v", err)
	}
	r, _ := s.LoadRun(context.Background(), "run-lease")
	if r.Status != store.RunStatusFinished {
		t.Fatalf("expected finished, got %s", r.Status)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, v := range violations {
		t.Errorf("lease violation: %s", v)
	}
	if maxConcurrent != len(pool) {
		t.Errorf("max concurrent leases = %d, want %d (pool not saturated, or over-leased)", maxConcurrent, len(pool))
	}
	inPool := map[string]bool{}
	for _, p := range pool {
		inPool[p] = true
	}
	for id := range seen {
		if !inPool[id] {
			t.Errorf("leased id %q is not a declared pool member %v", id, pool)
		}
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
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{
			item("A"), item("B", "A"), item("C", "A"), item("D", "B", "C"),
		}}, nil
	})
	exec.on("handle", func(input map[string]any) (map[string]any, error) {
		id := input["id"].(string)
		record(id + ":start")
		if id == "B" || id == "C" {
			// Force B and C to overlap: each arrives, the 2nd releases both.
			if atomic.AddInt64(&arrived, 1) == 2 {
				close(release)
			}
			select {
			case <-release:
			case <-time.After(10 * time.Second):
				t.Errorf("siblings B/C did not run concurrently (barrier timed out)")
			}
		}
		record(id + ":end")
		return map[string]any{"ok": true}, nil
	})
	exec.on("collect", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
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
	if idx("A:end") >= idx("B:start") || idx("A:end") >= idx("C:start") {
		t.Errorf("A must finish before B/C start; order=%v", order)
	}
	// Dependency: D starts only after B and C end.
	if idx("D:start") <= idx("B:end") || idx("D:start") <= idx("C:end") {
		t.Errorf("D must start after B/C end; order=%v", order)
	}
}

// TestFanOutEach_DAG_BoundedParallelism: 4 independent items, cap 2 →
// at most 2 run concurrently.
func TestFanOutEach_DAG_BoundedParallelism(t *testing.T) {
	wf := fanOutEachWorkflow(true, ir.AwaitWaitAll, 2)
	var maxC, curC int64
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{item("A"), item("B"), item("C"), item("D")}}, nil
	})
	exec.on("handle", func(_ map[string]any) (map[string]any, error) {
		cur := atomic.AddInt64(&curC, 1)
		for {
			old := atomic.LoadInt64(&maxC)
			if cur <= old || atomic.CompareAndSwapInt64(&maxC, old, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		atomic.AddInt64(&curC, -1)
		return map[string]any{}, nil
	})
	exec.on("collect", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
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
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{item("A"), item("B", "A")}}, nil
	})
	exec.on("handle", func(input map[string]any) (map[string]any, error) {
		id := input["id"].(string)
		if id == "A" {
			return nil, errors.New("A boom")
		}
		atomic.AddInt64(&bRan, 1) // B must never get here
		return map[string]any{}, nil
	})
	exec.on("collect", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
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
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{item("A", "B"), item("B", "A")}}, nil
	})
	exec.on("handle", func(_ map[string]any) (map[string]any, error) {
		atomic.AddInt64(&handleRan, 1)
		return map[string]any{}, nil
	})
	exec.on("collect", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
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

func TestFanOutEachWorkspaceSafetyRejectsConcurrentMutatingTemplate(t *testing.T) {
	wf := fanOutEachWorkflow(false, ir.AwaitWaitAll, 0)
	wf.Nodes["handle"] = &ir.ToolNode{BaseNode: ir.BaseNode{ID: "handle"}, Command: "touch shared"}

	var handleCalls int64
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{item("A"), item("B")}}, nil
	})
	exec.on("handle", func(_ map[string]any) (map[string]any, error) {
		atomic.AddInt64(&handleCalls, 1)
		return map[string]any{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	err := eng.Run(context.Background(), "run-fe-mutating-reject", nil)
	if err == nil {
		t.Fatal("expected workspace safety error")
	}
	var rtErr *RuntimeError
	if !errors.As(err, &rtErr) || rtErr.Code != ErrCodeWorkspaceSafety {
		t.Fatalf("expected RuntimeError ErrCodeWorkspaceSafety, got %T: %v", err, err)
	}
	if got := atomic.LoadInt64(&handleCalls); got != 0 {
		t.Fatalf("mutating template executed %d time(s), want 0 after safety rejection", got)
	}
}

func TestFanOutEachWorkspaceSafetyAllowsMutatingTemplateWhenSequential(t *testing.T) {
	wf := fanOutEachWorkflow(false, ir.AwaitWaitAll, 1)
	wf.Nodes["handle"] = &ir.ToolNode{BaseNode: ir.BaseNode{ID: "handle"}, Command: "touch shared"}

	var handleCalls int64
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{item("A"), item("B")}}, nil
	})
	exec.on("handle", func(_ map[string]any) (map[string]any, error) {
		atomic.AddInt64(&handleCalls, 1)
		return map[string]any{}, nil
	})
	exec.on("collect", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-fe-mutating-sequential", nil); err != nil {
		t.Fatalf("sequential mutating fan_out_each should be allowed, got %v", err)
	}
	if got := atomic.LoadInt64(&handleCalls); got != 2 {
		t.Fatalf("handle calls = %d, want 2", got)
	}
}

func TestFanOutEachWorkspaceSafetyAllowsReadonlyTemplateInParallel(t *testing.T) {
	wf := fanOutEachWorkflow(false, ir.AwaitWaitAll, 0)
	wf.Nodes["handle"] = &ir.AgentNode{
		BaseNode:  ir.BaseNode{ID: "handle"},
		LLMFields: ir.LLMFields{Readonly: true},
		Tools:     []string{"bash"},
	}

	var handleCalls int64
	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{item("A"), item("B")}}, nil
	})
	exec.on("handle", func(_ map[string]any) (map[string]any, error) {
		atomic.AddInt64(&handleCalls, 1)
		return map[string]any{}, nil
	})
	exec.on("collect", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{}, nil
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-fe-readonly-parallel", nil); err != nil {
		t.Fatalf("readonly fan_out_each template should be allowed in parallel, got %v", err)
	}
	if got := atomic.LoadInt64(&handleCalls); got != 2 {
		t.Fatalf("handle calls = %d, want 2", got)
	}
}

func TestFanOutEachInternalCancellationAbandonsWedgedBranch(t *testing.T) {
	oldGrace := branchCancelGracePeriod
	branchCancelGracePeriod = 100 * time.Millisecond
	defer func() { branchCancelGracePeriod = oldGrace }()

	wf := fanOutEachWorkflow(false, ir.AwaitWaitAll, 2)
	wedgedStarted := make(chan struct{})
	release := make(chan struct{})
	var closeOnce sync.Once

	exec := newStubExecutor()
	exec.on("entry", func(_ map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{item("A"), item("B")}}, nil
	})
	exec.on("handle", func(input map[string]any) (map[string]any, error) {
		id := input["id"].(string)
		if id == "B" {
			closeOnce.Do(func() { close(wedgedStarted) })
			<-release // deliberately ignores ctx until the test releases it
			return map[string]any{}, nil
		}
		<-wedgedStarted // ensure the sibling is already wedged before failing
		return nil, errors.New("A failed")
	})

	s := tmpStore(t)
	eng := New(wf, s, exec)

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- eng.Run(context.Background(), "run-fe-internal-cancel-wedged", nil) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected wait_all branch failure")
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("fan_out_each returned too slowly after internal cancellation: %s", elapsed)
		}
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("fan_out_each hung on a wedged branch after internal cancellation")
	}
	close(release)
	waitBranchFinished(t, s, "run-fe-internal-cancel-wedged", "branch_dispatch_1")
}
