package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// These tests cover the "loop head's fed-back value freezes at the loop-entry
// value" bug documented in bots/whole-improve-loop/main.bot (schema
// advance_state, ~line 611: "reading outputs.next_item on a next_item→…→
// next_item back-edge returns a stale loop-entry-frozen value").
//
// Root cause: buildNodeInputRS merged ALL incoming edges' with-mappings in
// e.workflow.Edges slice order (last-wins per key). A bounded-loop HEAD is
// targeted by BOTH an entry edge and a back-edge. On re-entry both sources
// have produced output, so whichever edge sits later in the slice clobbered
// the other for a shared key. When the entry edge lands last, it re-applies
// the loop-ENTRY value every iteration — freezing a fed-back cursor/counter.
//
// Fix: bounded-iteration back-edges (loop/foreach) are applied LAST so they
// win a shared key on re-entry; on first entry their source hasn't run so the
// entry edge still supplies the value.

// headExecutor wires the stub `head`/`seed` nodes. `head` reads input.c, emits
// c_next = c+1, c_now = c and done = c >= target, recording the c it saw.
func headExecutor(t *testing.T, target int) (*stubExecutor, *[]int64) {
	t.Helper()
	var mu sync.Mutex
	seen := &[]int64{}
	exec := newStubExecutor()
	exec.on("head", func(in map[string]any) (map[string]any, error) {
		c := toInt64(in["c"])
		mu.Lock()
		*seen = append(*seen, c)
		mu.Unlock()
		return map[string]any{"c_next": c + 1, "c_now": c, "done": c >= int64(target)}, nil
	})
	exec.on("seed", func(map[string]any) (map[string]any, error) {
		return map[string]any{"zero": int64(0)}, nil
	})
	return exec, seen
}

func toInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	default:
		return 0
	}
}

func equalInt64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// selfRefLoopWorkflow mirrors the bot: a loop HEAD `head` carries a cursor
// forward on its back-edge while ALSO being fed a one-time value by an entry
// edge from `seed`. The entry edge is declared AFTER the back-edge so, before
// the fix, its stale seed value (0) clobbers the advancing back-edge value.
//
//	body -> head  as spin(bound) with { c: "{{outputs.head.c_next}}" }   (back-edge, first in slice)
//	seed -> head                 with { c: "{{outputs.seed.zero}}"  }    (entry edge, last in slice)
//	head -> stop  when done
//	head -> body                                                          (cycle body)
func selfRefLoopWorkflow(bound int) *ir.Workflow {
	c := func(node, field string) *ir.DataMapping {
		return &ir.DataMapping{Key: "c", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{node, field}}}, Raw: "{{outputs." + node + "." + field + "}}"}
	}
	return &ir.Workflow{
		Name:  "selfref_loop",
		Entry: "seed",
		Nodes: map[string]ir.Node{
			"seed": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "seed"}},
			"head": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "head"}},
			"body": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "body"}},
			"stop": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "stop"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "body", To: "head", LoopName: "spin", With: []*ir.DataMapping{c("head", "c_next")}},
			{From: "seed", To: "head", With: []*ir.DataMapping{c("seed", "zero")}},
			{From: "head", To: "stop", Condition: "done"},
			{From: "head", To: "body"},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{"c": {Name: "c", Type: ir.VarInt, HasDefault: true, Default: int64(0)}},
		Loops: map[string]*ir.Loop{
			"spin": {Name: "spin", MaxIterations: bound, Entries: map[string]bool{"head": true}, Body: map[string]bool{"head": true, "body": true}},
		},
	}
}

// controlLoopWorkflow is the known-good form the bot relies on: the loop HEAD
// is the entry and ALL its incoming edges are the loop back-edge — there is no
// competing non-loop entry edge to clobber the fed-back key. The head carries
// its own cursor forward on the back-edge. This already converged before the
// fix; it is kept as a regression guard that the two-pass merge leaves the
// no-conflict case identical.
//
//	head -> stop  when done
//	head -> head  as spin(bound) with { c: "{{outputs.head.c_next}}" }
func controlLoopWorkflow(bound int) *ir.Workflow {
	return &ir.Workflow{
		Name:  "control_loop",
		Entry: "head",
		Nodes: map[string]ir.Node{
			"head": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "head"}},
			"stop": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "stop"}},
			"fail": &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "head", To: "stop", Condition: "done"},
			{From: "head", To: "head", LoopName: "spin", With: []*ir.DataMapping{
				{Key: "c", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"head", "c_next"}}}, Raw: "{{outputs.head.c_next}}"},
			}},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{"c": {Name: "c", Type: ir.VarInt, HasDefault: true, Default: int64(0)}},
		Loops: map[string]*ir.Loop{
			"spin": {Name: "spin", MaxIterations: bound, Entries: map[string]bool{"head": true}, Body: map[string]bool{"head": true}},
		},
	}
}

// runLoop drives a loop workflow to completion and returns the c-sequence the
// head observed. The engine error (if any) is returned, not fataled, so a
// freeze — which surfaces as loop exhaustion → NO_OUTGOING_EDGE — is asserted
// via the observed sequence rather than hiding behind a Fatalf.
func runLoop(t *testing.T, wf *ir.Workflow, target int, id string) ([]int64, string) {
	t.Helper()
	exec, seen := headExecutor(t, target)
	s := tmpStore(t)
	eng := New(wf, s, exec)
	_ = eng.Run(context.Background(), id, nil)
	r, _ := s.LoadRun(context.Background(), id)
	return *seen, string(r.Status)
}

// TestLoopHeadSelfRefBackEdgeAdvances is the bug repro. Before the fix the loop
// head's fed-back cursor freezes at the loop-entry value (0) and the loop spins
// to its bound (surfacing as NO_OUTGOING_EDGE once exhausted); after the fix it
// advances 0,1,2,3 and the run finishes.
func TestLoopHeadSelfRefBackEdgeAdvances(t *testing.T) {
	const bound = 10 // small so a regression is a bounded failure, never a hang
	const target = 3
	got, status := runLoop(t, selfRefLoopWorkflow(bound), target, "run-selfref")

	if len(got) > bound+2 { // hard cap: a runaway can never wedge CI
		t.Fatalf("head executed %d times (runaway) — cursor never advanced; saw=%v", len(got), got)
	}
	if want := []int64{0, 1, 2, 3}; !equalInt64(got, want) {
		t.Fatalf("loop-head fed-back cursor FROZE: head saw c=%v, want %v (status=%s)", got, want, status)
	}
	if status != string(store.RunStatusFinished) {
		t.Fatalf("expected finished, got %s (head saw c=%v)", status, got)
	}
}

// TestLoopHeadNoConflictLoopStillAdvances is the control: a loop head whose only
// incoming edge is its own back-edge (no competing entry edge). It converged
// before the fix and must still converge after — the two-pass merge is a no-op
// when there is no shared-key conflict.
func TestLoopHeadNoConflictLoopStillAdvances(t *testing.T) {
	const bound = 10
	const target = 3
	got, status := runLoop(t, controlLoopWorkflow(bound), target, "run-control")

	if len(got) > bound+2 {
		t.Fatalf("control: head executed %d times (runaway); saw=%v", len(got), got)
	}
	if want := []int64{0, 1, 2, 3}; !equalInt64(got, want) {
		t.Fatalf("control no-conflict loop FROZE: head saw c=%v, want %v (status=%s)", got, want, status)
	}
	if status != string(store.RunStatusFinished) {
		t.Fatalf("control: expected finished, got %s", status)
	}
}
