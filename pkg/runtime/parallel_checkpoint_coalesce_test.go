package runtime

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// countingSaves counts durable checkpoint writes without changing what
// lands. Write-count witnesses read the counter; state witnesses read
// through to the wrapped store.
type countingSaves struct {
	store.RunStore
	saves atomic.Int64
}

func (c *countingSaves) SaveCheckpoint(ctx context.Context, id string, cp *store.Checkpoint) error {
	c.saves.Add(1)
	return c.RunStore.SaveCheckpoint(ctx, id, cp)
}

// fanOutWithBranchLoop builds entry -> router(fan_out_all) -> branch a:
// a1 -> j1, with a bounded retry loop j1 --retry--> a1 (MaxIterations 2,
// the judge always says "again", so the bound force-exits) -> finalize;
// branch b: b1 -> b2 -> finalize; finalize(wait_all) -> done.
//
// Routine boundaries fire ONLY on bounded-iteration back-edges, so branch a
// carries exactly one (the j1->a1 crossing); branch b carries none and its
// only write is its completion.
func fanOutWithBranchLoop() *ir.Workflow {
	return &ir.Workflow{
		Name:  "fanout_coalesce_test",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"router":   &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"a1":       &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a1"}},
			"j1":       &ir.AgentNode{BaseNode: ir.BaseNode{ID: "j1"}},
			"b1":       &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b1"}},
			"b2":       &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b2"}},
			"finalize": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "finalize"}, AwaitMode: ir.AwaitWaitAll},
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "a1"},
			{From: "router", To: "b1"},
			{From: "a1", To: "j1"},
			{From: "j1", To: "a1", LoopName: "retry", Condition: "again"},
			{From: "j1", To: "finalize", IsElse: true},
			{From: "b1", To: "b2"},
			{From: "b2", To: "finalize"},
			{From: "finalize", To: "done"},
		},
		Loops: map[string]*ir.Loop{
			"retry": {Name: "retry", MaxIterations: 2, Entries: map[string]bool{"a1": true}, Body: map[string]bool{"a1": true, "j1": true}},
		},
		Foreaches: map[string]*ir.Foreach{},
		Schemas:   map[string]*ir.Schema{},
		Prompts:   map[string]*ir.Prompt{},
		Vars:      map[string]*ir.Var{},
	}
}

// stubFanOutNodes answers every node; j1 always asks for one more loop
// iteration, so the retry bound decides the exit deterministically.
func stubFanOutNodes(exec *stubExecutor) {
	outputs := map[string]map[string]any{
		"entry":    {"ok": true},
		"a1":       {"ok": true},
		"j1":       {"again": true},
		"b1":       {"ok": true},
		"b2":       {"ok": true},
		"finalize": {"ok": true},
	}
	for id, out := range outputs {
		exec.on(id, func(_ map[string]any) (map[string]any, error) {
			return out, nil
		})
	}
}

// TestFanOutSyncWindowKeepsPerBoundarySaves pins the escape hatch: with a
// zero window every boundary writes inline, one durable record per
// boundary, exactly as before coalescing existed. The exact composition:
// the entry trunk write, the invocation initialiser, branch a's TWO loop
// boundaries (the j1->a1 back-edge crossing and the bound exit, whose
// counters must be durable before the else path), branch a's completion,
// branch b's completion, and the finalize trunk write.
func TestFanOutSyncWindowKeepsPerBoundarySaves(t *testing.T) {
	counts := &countingSaves{RunStore: tmpStore(t)}
	exec := newStubExecutor()
	stubFanOutNodes(exec)
	eng := New(fanOutWithBranchLoop(), counts, exec, WithParallelSaveWindow(0))
	if err := eng.Run(context.Background(), "run-sync-hatch", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := counts.saves.Load(); got != 7 {
		t.Fatalf("sync window: got %d checkpoint writes, want 7 (entry trunk + init + 2 loop boundaries + 2 completions + finalize trunk)", got)
	}
}

// TestParallelSaveWindowEnvOverride pins the operator's off-switch:
// ITERION_PARALLEL_SAVE_WINDOW=0 disables coalescing at construction and an
// unparsable value falls back instead of guessing.
func TestParallelSaveWindowEnvOverride(t *testing.T) {
	t.Setenv("ITERION_PARALLEL_SAVE_WINDOW", "0")
	eng := New(fanOutWithBranchLoop(), tmpStore(t), newStubExecutor())
	if eng.parallelSaveWindow != 0 {
		t.Fatalf("env window 0: got %v, want 0 (coalescing disabled)", eng.parallelSaveWindow)
	}
	t.Setenv("ITERION_PARALLEL_SAVE_WINDOW", "not-a-duration")
	eng = New(fanOutWithBranchLoop(), tmpStore(t), newStubExecutor())
	if eng.parallelSaveWindow != fallbackParallelSaveWindow {
		t.Fatalf("unparsable env window: got %v, want the %v fallback", eng.parallelSaveWindow, fallbackParallelSaveWindow)
	}
}

// TestFanOutCoalescerStateMachine drives the invocation's save state
// machine directly: one flush per window covers every boundary kicked
// within it, a write that lands between kick and window suppresses the
// flush, and retire flushes a boundary that never met its window.
// testing/synctest moves the window's timer without a real sleep — a sleep
// is what advances virtual time (Wait alone does not), and Wait then
// returns only when every goroutine in the bubble is durably blocked.
func TestFanOutCoalescerStateMachine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := &parallelExecutionState{cp: &store.ParallelCheckpoint{}}
		var flushes atomic.Int64
		// The fake flush mirrors the real one's generation bookkeeping: the
		// production flush marks the invocation written when the store write
		// succeeds.
		p.flush = func() { flushes.Add(1); p.markWritten() }
		p.saveWindow = 50 * time.Millisecond
		p.saveKick = make(chan struct{}, 1)
		p.saveStop = make(chan struct{})
		p.saveDone = make(chan struct{})
		go p.saverLoop(context.Background())
		// A failing assertion must not strand the saver (the bubble would
		// panic on its blocked goroutine); retire stops it idempotently.
		defer p.retire()

		e := &Engine{}
		// Two boundaries inside one window share ONE flush: the parked kick
		// drops the second send, and the window's close covers both.
		e.requestParallelSave(nil, p, "a", "a1")
		e.requestParallelSave(nil, p, "b", "b1")
		time.Sleep(60 * time.Millisecond)
		synctest.Wait()
		if got := flushes.Load(); got != 1 {
			t.Fatalf("one window: %d flushes, want 1", got)
		}
		p.mu.Lock()
		written := p.writtenGen
		p.mu.Unlock()
		if written != 2 {
			t.Fatalf("written generation %d, want 2 (both boundaries covered)", written)
		}

		// A write that lands between the kick and the window (a completion,
		// a spend) covers the pending boundary; the saver's flush then finds
		// nothing pending — no record repeats a covered write.
		e.requestParallelSave(nil, p, "a", "a2")
		p.markWritten()
		time.Sleep(60 * time.Millisecond)
		synctest.Wait()
		if got := flushes.Load(); got != 1 {
			t.Fatalf("a covered boundary must not flush again: %d flushes, want 1", got)
		}

		// A boundary that never meets its window flushes at retire.
		e.requestParallelSave(nil, p, "b", "b2")
		p.retire()
		if got := flushes.Load(); got != 2 {
			t.Fatalf("retire: %d flushes, want 2 (pending boundary flushed)", got)
		}
	})
}

// TestFanOutCoalescedEndMatchesLegacy runs the loop-bearing fan-out with
// and without the save window and certifies the async plumbing end to end:
// the crossing that rode kick -> saver -> flush lands durably just like the
// synchronous one — both branches complete at the same cursors — and
// coalescing never writes more records than the legacy path. (Whether two
// boundaries merge inside one window is real-clock timing; the merge itself
// is pinned deterministically by the state-machine test above.)
func TestFanOutCoalescedEndMatchesLegacy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type outcome struct {
			saves   int64
			cursorA string
			cursorB string
			doneA   bool
			doneB   bool
		}
		run := func(window time.Duration) outcome {
			counts := &countingSaves{RunStore: tmpStore(t)}
			exec := newStubExecutor()
			stubFanOutNodes(exec)
			// The parallel section is only observable while the invocation
			// is alive: observe it from the finalize node, which runs after
			// the collector retired every branch but before the run's own
			// final record drops the section.
			var observed *store.ParallelCheckpoint
			exec.on("finalize", func(_ map[string]any) (map[string]any, error) {
				if r, err := counts.LoadRun(context.Background(), "run-"+window.String()); err == nil && r.Checkpoint != nil {
					observed = r.Checkpoint.Parallel
				}
				return map[string]any{"ok": true}, nil
			})
			eng := New(fanOutWithBranchLoop(), counts, exec, WithParallelSaveWindow(window))
			if err := eng.Run(context.Background(), "run-"+window.String(), nil); err != nil {
				t.Fatalf("run with window %v: %v", window, err)
			}
			if observed == nil {
				t.Fatalf("window %v: no parallel checkpoint observable at finalize", window)
			}
			out := outcome{saves: counts.saves.Load()}
			// Branch ids are branch_<router>_<target edge> (fan_out.go's
			// starts map).
			if b := observed.Branches["branch_router_a1"]; b != nil {
				out.cursorA, out.doneA = b.CurrentNodeID, b.Completed
			}
			if b := observed.Branches["branch_router_b1"]; b != nil {
				out.cursorB, out.doneB = b.CurrentNodeID, b.Completed
			}
			return out
		}

		coalesced := run(50 * time.Millisecond)
		legacy := run(0)

		if !coalesced.doneA || !coalesced.doneB {
			t.Fatalf("coalesced run lost a completion: %+v", coalesced)
		}
		if coalesced.cursorA != legacy.cursorA || coalesced.cursorB != legacy.cursorB {
			t.Fatalf("coalesced cursors drifted from legacy: coalesced=(%s,%s) legacy=(%s,%s)",
				coalesced.cursorA, coalesced.cursorB, legacy.cursorA, legacy.cursorB)
		}
		if coalesced.saves > legacy.saves {
			t.Fatalf("coalescing wrote MORE records than legacy: %d > %d", coalesced.saves, legacy.saves)
		}
	})
}

// tmpStoreB is tmpStore for benchmarks.
func tmpStoreB(b *testing.B) store.RunStore {
	b.Helper()
	s, err := store.New(b.TempDir())
	if err != nil {
		b.Fatalf("create store: %v", err)
	}
	return s
}

// wideFanOut builds entry -> router -> branches x steps agent nodes, where
// each branch's last node loops back to its first (bounded, the stub always
// says "again") before taking the else edge to finalize(wait_all) -> done.
// Every branch therefore carries a routine boundary per run — the shape the
// checkpoint cost grows with.
func wideFanOut(branches, steps int) *ir.Workflow {
	nodes := map[string]ir.Node{
		"entry":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
		"router":   &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
		"finalize": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "finalize"}, AwaitMode: ir.AwaitWaitAll},
		"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
	}
	edges := []*ir.Edge{{From: "entry", To: "router"}}
	loops := map[string]*ir.Loop{}
	for bi := 0; bi < branches; bi++ {
		first := fmt.Sprintf("b%d_s1", bi)
		last := fmt.Sprintf("b%d_s%d", bi, steps)
		prev := "router"
		for si := 1; si <= steps; si++ {
			id := fmt.Sprintf("b%d_s%d", bi, si)
			nodes[id] = &ir.AgentNode{BaseNode: ir.BaseNode{ID: id}}
			edges = append(edges, &ir.Edge{From: prev, To: id})
			prev = id
		}
		loopName := fmt.Sprintf("lp%d", bi)
		edges = append(edges, &ir.Edge{From: last, To: first, LoopName: loopName, Condition: "again"})
		edges = append(edges, &ir.Edge{From: last, To: "finalize", IsElse: true})
		entrySet := map[string]bool{first: true}
		bodySet := map[string]bool{}
		for si := 1; si <= steps; si++ {
			bodySet[fmt.Sprintf("b%d_s%d", bi, si)] = true
		}
		loops[loopName] = &ir.Loop{Name: loopName, MaxIterations: 2, Entries: entrySet, Body: bodySet}
	}
	edges = append(edges, &ir.Edge{From: "finalize", To: "done"})
	return &ir.Workflow{
		Name: "fanout_wide_bench", Entry: "entry", Nodes: nodes, Edges: edges,
		Loops:     loops,
		Foreaches: map[string]*ir.Foreach{},
		Schemas:   map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{},
		Vars: map[string]*ir.Var{},
	}
}

// BenchmarkFanOutCheckpointSaves scales the fan-out (8 branches of 4 nodes,
// each with a bounded loop) and reports the durable checkpoint writes per
// run — the asymptotic term the coalescer removes — alongside wall time.
func BenchmarkFanOutCheckpointSaves(b *testing.B) {
	for _, window := range []time.Duration{0, time.Millisecond} {
		b.Run(fmt.Sprintf("window=%v", window), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				counts := &countingSaves{RunStore: tmpStoreB(b)}
				exec := newStubExecutor()
				exec.on("entry", func(_ map[string]any) (map[string]any, error) {
					return map[string]any{"ok": true}, nil
				})
				for bi := 0; bi < 8; bi++ {
					for si := 1; si <= 4; si++ {
						id := fmt.Sprintf("b%d_s%d", bi, si)
						out := map[string]any{"ok": true}
						if si == 4 {
							out = map[string]any{"again": true}
						}
						exec.on(id, func(_ map[string]any) (map[string]any, error) {
							return out, nil
						})
					}
				}
				exec.on("finalize", func(_ map[string]any) (map[string]any, error) {
					return map[string]any{"ok": true}, nil
				})
				eng := New(wideFanOut(8, 4), counts, exec, WithParallelSaveWindow(window))
				b.StartTimer()
				if err := eng.Run(context.Background(), fmt.Sprintf("bench-%v-%d", window, i), nil); err != nil {
					b.Fatalf("run: %v", err)
				}
				b.ReportMetric(float64(counts.saves.Load()), "checkpoint-writes/run")
			}
		})
	}
}
