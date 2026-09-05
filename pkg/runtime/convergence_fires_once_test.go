package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// countingExecutor records every Execute per node id under a mutex — the
// branches of a fan-out call it concurrently — and runs an optional hook per
// node. The default output carries the node id under "from" so a collector's
// with-mappings can prove which branches reached it.
type countingExecutor struct {
	mu     sync.Mutex
	calls  map[string]int
	inputs map[string][]map[string]any
	hooks  map[string]func(map[string]any) (map[string]any, error)
}

func newCountingExecutor() *countingExecutor {
	return &countingExecutor{
		calls:  map[string]int{},
		inputs: map[string][]map[string]any{},
		hooks:  map[string]func(map[string]any) (map[string]any, error){},
	}
}

func (c *countingExecutor) on(nodeID string, fn func(map[string]any) (map[string]any, error)) {
	c.hooks[nodeID] = fn
}

func (c *countingExecutor) Execute(_ context.Context, node ir.Node, input map[string]any) (map[string]any, error) {
	id := node.NodeID()
	c.mu.Lock()
	c.calls[id]++
	c.inputs[id] = append(c.inputs[id], input)
	hook := c.hooks[id]
	c.mu.Unlock()
	if hook != nil {
		return hook(input)
	}
	return map[string]any{"from": id}, nil
}

func (c *countingExecutor) count(nodeID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[nodeID]
}

func (c *countingExecutor) lastInput(nodeID string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n := len(c.inputs[nodeID]); n > 0 {
		return c.inputs[nodeID][n-1]
	}
	return nil
}

func outputsRef(node string) []*ir.DataMapping {
	raw := "{{outputs." + node + "}}"
	return []*ir.DataMapping{{Key: node, Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{node}, Raw: raw}}, Raw: raw}}
}

// sharedTargetFanOut is the shape review-pr and evolve ship: a trunk node
// routes either DIRECTLY to `a` (mono) or to a fan_out_all whose targets are
// `a` and `b` (dual). Both paths converge on `merge`, the declared collector,
// then `tail`, then done. `a` therefore has two distinct predecessors — the
// trunk and the fan-out — only one of which belongs to the fan-out.
func sharedTargetFanOut(await ir.AwaitMode) *ir.Workflow {
	return &ir.Workflow{
		Name:  "shared_target_fan_out",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"fan":   &ir.RouterNode{BaseNode: ir.BaseNode{ID: "fan"}, RouterMode: ir.RouterFanOutAll},
			"a":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "a"}},
			"b":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "b"}},
			"merge": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "merge"}, AwaitMode: await},
			"tail":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "tail"}},
			"done":  &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":  &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "fan", Condition: "dual"},
			{From: "entry", To: "a", Condition: "dual", Negated: true},
			{From: "fan", To: "a"},
			{From: "fan", To: "b"},
			{From: "a", To: "merge", With: outputsRef("a")},
			{From: "b", To: "merge", With: outputsRef("b")},
			{From: "merge", To: "tail"},
			{From: "tail", To: "done"},
		},
		Schemas:   map[string]*ir.Schema{},
		Prompts:   map[string]*ir.Prompt{},
		Vars:      map[string]*ir.Var{},
		Loops:     map[string]*ir.Loop{},
		Foreaches: map[string]*ir.Foreach{},
	}
}

func dualEntry(dual bool) func(map[string]any) (map[string]any, error) {
	return func(map[string]any) (map[string]any, error) {
		return map[string]any{"dual": dual}, nil
	}
}

// nodeStartedCounts is the blind judge: it reads the persisted event log,
// not the executor, so compute nodes and trunk/branch executions count alike.
func nodeStartedCounts(t *testing.T, s store.RunStore, runID string) (map[string]int, []*store.Event) {
	t.Helper()
	events, err := s.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	started := map[string]int{}
	var joins []*store.Event
	for _, evt := range events {
		switch evt.Type {
		case store.EventNodeStarted:
			started[evt.NodeID]++
		case store.EventJoinReady:
			joins = append(joins, evt)
		}
	}
	return started, joins
}

func assertRunStatus(t *testing.T, s store.RunStore, runID string, want store.RunStatus) {
	t.Helper()
	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if r.Status != want {
		t.Fatalf("status = %s, want %s", r.Status, want)
	}
}

func assertCounts(t *testing.T, exec *countingExecutor, started map[string]int, want map[string]int) {
	t.Helper()
	for id, n := range want {
		if got := exec.count(id); got != n {
			t.Errorf("executor calls for %s = %d, want %d", id, got, n)
		}
		if got := started[id]; got != n {
			t.Errorf("node_started events for %s = %d, want %d", id, got, n)
		}
	}
}

func assertSingleJoin(t *testing.T, joins []*store.Event, nodeID string, strategy ir.AwaitMode) {
	t.Helper()
	if len(joins) != 1 {
		t.Fatalf("join_ready events = %d, want exactly 1", len(joins))
	}
	if joins[0].NodeID != nodeID {
		t.Errorf("join_ready node = %s, want %s", joins[0].NodeID, nodeID)
	}
	if got := joins[0].Data["strategy"]; got != strategy.String() {
		t.Errorf("join_ready strategy = %v, want %s", got, strategy)
	}
}

// The collector of a fan-out fires exactly once, after every branch has
// settled — under both await modes, and regardless of whether the branches
// complete back-to-back (a synchronous stub) or spread apart. A fan-out
// target that ALSO has a predecessor outside the fan-out (the mono path)
// must not be mistaken for the collector: that election makes its branch
// stop before executing anything, lets the sibling run the whole
// post-fan-out chain inside its branch, and then runs the same chain a
// second time on the trunk.
func TestSharedTargetFanOut_CollectorFiresOnce(t *testing.T) {
	for _, await := range []ir.AwaitMode{ir.AwaitBestEffort, ir.AwaitWaitAll} {
		for _, async := range []bool{false, true} {
			name := await.String()
			if async {
				name += "-async"
			}
			t.Run(name, func(t *testing.T) {
				wf := sharedTargetFanOut(await)
				exec := newCountingExecutor()
				exec.on("entry", dualEntry(true))
				if async {
					// `a` completes only after `b` has finished, so the two
					// arrivals at the collector are strictly ordered.
					bDone := make(chan struct{})
					exec.on("b", func(map[string]any) (map[string]any, error) {
						close(bDone)
						return map[string]any{"from": "b"}, nil
					})
					exec.on("a", func(map[string]any) (map[string]any, error) {
						<-bDone
						return map[string]any{"from": "a"}, nil
					})
				}
				s := tmpStore(t)
				runID := "shared-target-" + name
				if err := New(wf, s, exec).Run(context.Background(), runID, nil); err != nil {
					t.Fatalf("run: %v", err)
				}
				assertRunStatus(t, s, runID, store.RunStatusFinished)
				started, joins := nodeStartedCounts(t, s, runID)
				assertCounts(t, exec, started, map[string]int{"entry": 1, "a": 1, "b": 1, "merge": 1, "tail": 1})
				assertSingleJoin(t, joins, "merge", await)
				in := exec.lastInput("merge")
				for _, branch := range []string{"a", "b"} {
					out, _ := in[branch].(map[string]any)
					if out["from"] != branch {
						t.Errorf("merge input %s = %v, want the output of branch %s", branch, in[branch], branch)
					}
				}
			})
		}
	}
}

// Mono control: the same graph routed around the fan-out executes the
// collector once as an ordinary trunk node — no join, no second family.
func TestSharedTargetFanOut_MonoPathRunsCollectorOnceWithoutJoin(t *testing.T) {
	wf := sharedTargetFanOut(ir.AwaitBestEffort)
	exec := newCountingExecutor()
	exec.on("entry", dualEntry(false))
	s := tmpStore(t)
	if err := New(wf, s, exec).Run(context.Background(), "shared-target-mono", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	assertRunStatus(t, s, "shared-target-mono", store.RunStatusFinished)
	started, joins := nodeStartedCounts(t, s, "shared-target-mono")
	assertCounts(t, exec, started, map[string]int{"a": 1, "b": 0, "merge": 1, "tail": 1})
	if len(joins) != 0 {
		t.Errorf("join_ready events = %d, want none on the mono path", len(joins))
	}
}

// A failing branch under best_effort is tolerated: the collector still fires
// exactly once, with the survivor's output and the failure named on the
// join_ready event. Under wait_all the failure ends the run and the
// collector never fires — the documented behaviour, preserved.
func TestSharedTargetFanOut_BranchFailure(t *testing.T) {
	t.Run("best_effort tolerates the failure and fires once", func(t *testing.T) {
		wf := sharedTargetFanOut(ir.AwaitBestEffort)
		exec := newCountingExecutor()
		exec.on("entry", dualEntry(true))
		exec.on("a", func(map[string]any) (map[string]any, error) {
			return nil, errors.New("reviewer a exploded")
		})
		s := tmpStore(t)
		if err := New(wf, s, exec).Run(context.Background(), "shared-target-fail-be", nil); err != nil {
			t.Fatalf("run: %v", err)
		}
		assertRunStatus(t, s, "shared-target-fail-be", store.RunStatusFinished)
		started, joins := nodeStartedCounts(t, s, "shared-target-fail-be")
		assertCounts(t, exec, started, map[string]int{"a": 1, "b": 1, "merge": 1, "tail": 1})
		assertSingleJoin(t, joins, "merge", ir.AwaitBestEffort)
		// The event comes back from the store's JSON round trip, so the
		// list is []any of map[string]any.
		failed, _ := joins[0].Data["failed_branches"].([]any)
		if len(failed) != 1 {
			t.Fatalf("join_ready failed_branches = %v, want exactly branch_fan_a", joins[0].Data["failed_branches"])
		}
		if entry, _ := failed[0].(map[string]any); entry["branch_id"] != "branch_fan_a" {
			t.Errorf("join_ready failed branch = %v, want branch_fan_a", failed[0])
		}
		in := exec.lastInput("merge")
		if out, _ := in["b"].(map[string]any); out["from"] != "b" {
			t.Errorf("merge input b = %v, want the survivor's output", in["b"])
		}
	})
	t.Run("wait_all fails the run before the collector", func(t *testing.T) {
		wf := sharedTargetFanOut(ir.AwaitWaitAll)
		exec := newCountingExecutor()
		exec.on("entry", dualEntry(true))
		exec.on("a", func(map[string]any) (map[string]any, error) {
			return nil, errors.New("reviewer a exploded")
		})
		s := tmpStore(t)
		if err := New(wf, s, exec).Run(context.Background(), "shared-target-fail-wa", nil); err == nil {
			t.Fatal("run succeeded, want the wait_all failure")
		}
		assertRunStatus(t, s, "shared-target-fail-wa", store.RunStatusFailedResumable)
		started, joins := nodeStartedCounts(t, s, "shared-target-fail-wa")
		assertCounts(t, exec, started, map[string]int{"a": 1, "merge": 0, "tail": 0})
		if len(joins) != 0 {
			t.Errorf("join_ready events = %d, want none when wait_all fails", len(joins))
		}
	})
}

// A branch that pauses at a human gate resumes into the SAME fan-out: the
// completed sibling is not replayed and the collector fires once across the
// two engine invocations.
func TestSharedTargetFanOut_ResumeAfterBranchPauseFiresCollectorOnce(t *testing.T) {
	wf := sharedTargetFanOut(ir.AwaitBestEffort)
	wf.Nodes["gate"] = &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}}
	for _, edge := range wf.Edges {
		if edge.From == "fan" && edge.To == "b" {
			edge.To = "gate"
		}
	}
	wf.Edges = append(wf.Edges, &ir.Edge{From: "gate", To: "b", Condition: "approved"})

	exec := newCountingExecutor()
	exec.on("entry", dualEntry(true))
	s := tmpStore(t)
	runID := "shared-target-resume-pause"
	if err := New(wf, s, exec).Run(context.Background(), runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("run = %v, want ErrRunPaused", err)
	}
	if exec.count("a") != 1 {
		t.Fatalf("a ran %d times before the pause, want 1", exec.count("a"))
	}
	// A fresh engine simulates a restart: only the checkpoint carries the
	// fan-out across this boundary.
	if err := New(wf, s, exec).Resume(context.Background(), runID, map[string]any{"approved": true}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	assertRunStatus(t, s, runID, store.RunStatusFinished)
	started, joins := nodeStartedCounts(t, s, runID)
	assertCounts(t, exec, started, map[string]int{"a": 1, "b": 1, "merge": 1, "tail": 1})
	assertSingleJoin(t, joins, "merge", ir.AwaitBestEffort)
}

// A failure DOWNSTREAM of the collector resumes from the failed node: the
// checkpoint taken after the convergence must not re-fire it.
func TestSharedTargetFanOut_ResumeAfterTailFailureDoesNotRefireCollector(t *testing.T) {
	wf := sharedTargetFanOut(ir.AwaitBestEffort)
	exec := newCountingExecutor()
	exec.on("entry", dualEntry(true))
	var tailMu sync.Mutex
	tailCalls := 0
	exec.on("tail", func(map[string]any) (map[string]any, error) {
		tailMu.Lock()
		defer tailMu.Unlock()
		tailCalls++
		if tailCalls == 1 {
			return nil, errors.New("tail exploded once")
		}
		return map[string]any{"from": "tail"}, nil
	})
	s := tmpStore(t)
	runID := "shared-target-resume-tail"
	if err := New(wf, s, exec).Run(context.Background(), runID, nil); err == nil {
		t.Fatal("run succeeded, want the tail failure")
	}
	assertRunStatus(t, s, runID, store.RunStatusFailedResumable)
	if err := New(wf, s, exec).Resume(context.Background(), runID, nil); err != nil {
		t.Fatalf("resume: %v", err)
	}
	assertRunStatus(t, s, runID, store.RunStatusFinished)
	started, joins := nodeStartedCounts(t, s, runID)
	assertCounts(t, exec, started, map[string]int{"a": 1, "b": 1, "merge": 1, "tail": 2})
	assertSingleJoin(t, joins, "merge", ir.AwaitBestEffort)
}

// The same class on fan_out_each: a template head that is ALSO reachable
// from outside the fan-out (a trunk shortcut for the single-item case) must
// not be elected as the collector — that election skips every item replay
// and runs the template once, on the trunk, with no item bound.
func TestSharedTemplateHeadFanOutEach_ReplaysEveryItemAndFiresCollectorOnce(t *testing.T) {
	dispatch := &ir.RouterNode{
		BaseNode:    ir.BaseNode{ID: "dispatch"},
		RouterMode:  ir.RouterFanOutEach,
		Over:        "{{outputs.entry.items}}",
		OverRefs:    []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"entry", "items"}, Raw: "{{outputs.entry.items}}"}},
		ItemBinding: "item",
	}
	wf := &ir.Workflow{
		Name:  "shared_template_head",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"dispatch": dispatch,
			"work":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work"}},
			"collect":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "collect"}, AwaitMode: ir.AwaitBestEffort},
			"tail":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "tail"}},
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":     &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "dispatch", Condition: "many"},
			{From: "entry", To: "work", Condition: "many", Negated: true},
			{From: "dispatch", To: "work", With: []*ir.DataMapping{{
				Key:  "id",
				Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"dispatch", "item", "id"}, Raw: "{{outputs.dispatch.item.id}}"}},
				Raw:  "{{outputs.dispatch.item.id}}",
			}}},
			{From: "work", To: "collect"},
			{From: "collect", To: "tail"},
			{From: "tail", To: "done"},
		},
		Schemas:   map[string]*ir.Schema{},
		Prompts:   map[string]*ir.Prompt{},
		Vars:      map[string]*ir.Var{},
		Loops:     map[string]*ir.Loop{},
		Foreaches: map[string]*ir.Foreach{},
	}
	exec := newCountingExecutor()
	exec.on("entry", func(map[string]any) (map[string]any, error) {
		return map[string]any{"many": true, "items": []any{map[string]any{"id": "one"}, map[string]any{"id": "two"}}}, nil
	})
	var mu sync.Mutex
	seen := map[string]bool{}
	exec.on("work", func(input map[string]any) (map[string]any, error) {
		id, _ := input["id"].(string)
		mu.Lock()
		seen[id] = true
		mu.Unlock()
		return map[string]any{"id": id}, nil
	})
	s := tmpStore(t)
	if err := New(wf, s, exec).Run(context.Background(), "shared-template-head", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	assertRunStatus(t, s, "shared-template-head", store.RunStatusFinished)
	started, joins := nodeStartedCounts(t, s, "shared-template-head")
	assertCounts(t, exec, started, map[string]int{"work": 2, "collect": 1, "tail": 1})
	assertSingleJoin(t, joins, "collect", ir.AwaitBestEffort)
	mu.Lock()
	defer mu.Unlock()
	if !seen["one"] || !seen["two"] {
		t.Errorf("work saw items %v, want both one and two bound", seen)
	}
}
