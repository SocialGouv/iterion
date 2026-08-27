package runtime

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

// exclusiveSiblingWorkflow is the Copi-shaped graph that motivated #484:
//
//	copi -> validate when has_draft
//	copi -> gate else          with {validated: "from-else"}
//	validate -> gate           with {validated: "from-validate"}
//
// The two incoming edges into gate are mutually exclusive, but after the
// validation path both copi and validate have outputs. Before the fix,
// buildNodeInputRS applied both mappings and whichever sat last in
// workflow.Edges silently won.
func exclusiveSiblingWorkflow(elseLast bool) *ir.Workflow {
	elseEdge := &ir.Edge{
		From:   "copi",
		To:     "gate",
		IsElse: true,
		With:   []*ir.DataMapping{{Key: "validated", Raw: "from-else"}},
	}
	validateEdge := &ir.Edge{
		From: "validate",
		To:   "gate",
		With: []*ir.DataMapping{{Key: "validated", Raw: "from-validate"}},
	}
	edges := []*ir.Edge{
		{From: "copi", To: "validate", Condition: "has_draft"},
	}
	if elseLast {
		edges = append(edges, validateEdge, elseEdge)
	} else {
		edges = append(edges, elseEdge, validateEdge)
	}
	edges = append(edges, &ir.Edge{From: "gate", To: "done"})
	return &ir.Workflow{
		Name:  "exclusive_siblings",
		Entry: "copi",
		Nodes: map[string]ir.Node{
			"copi":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "copi"}},
			"validate": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "validate"}},
			"gate":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "gate"}},
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":     &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges:   edges,
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{},
	}
}

func runExclusiveSiblings(t *testing.T, wf *ir.Workflow, hasDraft bool, runID string) (gateInput map[string]any, status store.RunStatus) {
	t.Helper()
	var mu sync.Mutex
	exec := newStubExecutor()
	exec.on("copi", func(map[string]any) (map[string]any, error) {
		return map[string]any{"has_draft": hasDraft, "draft": "body"}, nil
	})
	exec.on("validate", func(map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true, "report": "looks good"}, nil
	})
	exec.on("gate", func(in map[string]any) (map[string]any, error) {
		mu.Lock()
		gateInput = map[string]any{}
		for k, v := range in {
			gateInput[k] = v
		}
		mu.Unlock()
		return map[string]any{"shown": true}, nil
	})
	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	r, err := s.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	return gateInput, r.Status
}

func TestSelectedIncoming_ExclusiveSiblingsIgnoreUnselectedElse(t *testing.T) {
	// The else edge is declared LAST so the pre-#484 last-wins merge
	// would overwrite the validation mapping with "from-else".
	got, status := runExclusiveSiblings(t, exclusiveSiblingWorkflow(true), true, "run-else-last")
	if status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished", status)
	}
	if got["validated"] != "from-validate" {
		t.Fatalf("gate.validated = %#v, want %q (unselected else mapping must not win)", got["validated"], "from-validate")
	}
}

func TestSelectedIncoming_ExclusiveSiblingsIgnoreUnselectedElseRegardlessOfOrder(t *testing.T) {
	// The Copi workaround: declare the else edge BEFORE validate→gate so
	// last-wins happened to pick the selected mapping. Routing, not slice
	// order, must be the authority.
	got, status := runExclusiveSiblings(t, exclusiveSiblingWorkflow(false), true, "run-else-first")
	if status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished", status)
	}
	if got["validated"] != "from-validate" {
		t.Fatalf("gate.validated = %#v, want %q", got["validated"], "from-validate")
	}
}

func TestSelectedIncoming_ElsePathStillApplies(t *testing.T) {
	got, status := runExclusiveSiblings(t, exclusiveSiblingWorkflow(true), false, "run-else-taken")
	if status != store.RunStatusFinished {
		t.Fatalf("status = %s, want finished", status)
	}
	if got["validated"] != "from-else" {
		t.Fatalf("gate.validated = %#v, want %q (the else edge was selected)", got["validated"], "from-else")
	}
}

func TestSelectedIncoming_UntrackedFallbackMergesAllWithOutput(t *testing.T) {
	wf := exclusiveSiblingWorkflow(true)
	eng := New(wf, tmpStore(t), newStubExecutor())
	rs := eng.newRunState("run-untracked", nil)
	rs.outputs["copi"] = map[string]any{"has_draft": true}
	rs.outputs["validate"] = map[string]any{"ok": true}
	// No selectedIncoming key for gate → pre-#484 fallback.
	got := eng.buildNodeInputRS("gate", rs.scope())
	if got["validated"] != "from-else" {
		t.Fatalf("untracked fallback last-wins = %#v, want %q (else declared last)", got["validated"], "from-else")
	}

	rs.selectedIncoming["gate"] = []store.IncomingEdge{{From: "validate", To: "gate"}}
	got = eng.buildNodeInputRS("gate", rs.scope())
	if got["validated"] != "from-validate" {
		t.Fatalf("tracked incoming = %#v, want %q", got["validated"], "from-validate")
	}
}

func TestSelectedIncoming_FanOutJoinMergesBothSelected(t *testing.T) {
	wf := fanOutWorkflow(ir.AwaitWaitAll)
	var captured map[string]any
	exec := newStubExecutor()
	exec.on("entry", func(map[string]any) (map[string]any, error) {
		return map[string]any{"summary": "PR"}, nil
	})
	exec.on("agent_a", func(map[string]any) (map[string]any, error) {
		return map[string]any{"review": "A"}, nil
	})
	exec.on("agent_b", func(map[string]any) (map[string]any, error) {
		return map[string]any{"review": "B"}, nil
	})
	exec.on("finalize", func(in map[string]any) (map[string]any, error) {
		captured = map[string]any{}
		for k, v := range in {
			captured[k] = v
		}
		return map[string]any{"ok": true}, nil
	})
	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-join", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if captured["review_a"] == nil {
		t.Fatalf("finalize missing review_a: %#v", captured)
	}
	if captured["review_b"] == nil {
		t.Fatalf("finalize missing review_b: %#v", captured)
	}
}

func TestSelectedIncoming_SurvivesFailedResume(t *testing.T) {
	wf := exclusiveSiblingWorkflow(true)
	call := 0
	var seen []any
	exec := newStubExecutor()
	exec.on("copi", func(map[string]any) (map[string]any, error) {
		return map[string]any{"has_draft": true, "draft": "body"}, nil
	})
	exec.on("validate", func(map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})
	exec.on("gate", func(in map[string]any) (map[string]any, error) {
		call++
		seen = append(seen, in["validated"])
		if call == 1 {
			return nil, fmt.Errorf("transient")
		}
		return map[string]any{"shown": true}, nil
	})
	s := tmpStore(t)
	eng := New(wf, s, exec)
	if err := eng.Run(context.Background(), "run-resume-sel", nil); err == nil {
		t.Fatal("expected gate to fail the first run")
	}
	r, err := s.LoadRun(context.Background(), "run-resume-sel")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if r.Status != store.RunStatusFailedResumable {
		t.Fatalf("status = %s, want failed_resumable", r.Status)
	}
	if r.Checkpoint == nil || len(r.Checkpoint.SelectedIncoming["gate"]) == 0 {
		t.Fatalf("checkpoint did not persist selected incoming for gate: %+v", r.Checkpoint)
	}
	foundValidate := false
	for _, in := range r.Checkpoint.SelectedIncoming["gate"] {
		if in.From == "validate" && in.To == "gate" && !in.IsElse {
			foundValidate = true
		}
		if in.IsElse {
			t.Fatalf("checkpoint selected the unselected else edge: %+v", in)
		}
	}
	if !foundValidate {
		t.Fatalf("checkpoint selected incoming for gate = %+v, want validate→gate", r.Checkpoint.SelectedIncoming["gate"])
	}

	if err := eng.Resume(context.Background(), "run-resume-sel", nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if call != 2 {
		t.Fatalf("gate called %d times, want 2", call)
	}
	for i, v := range seen {
		if v != "from-validate" {
			t.Fatalf("gate visit %d validated = %#v, want %q (selection must survive resume)", i+1, v, "from-validate")
		}
	}
}

func TestSelectedIncoming_LoopReentryOverlaysPartialBackEdge(t *testing.T) {
	// A back-edge restates only the keys that change. On re-entry the
	// recorded set is that single back-edge; unmapped keys must still
	// come from the entry edge (Rae4900).
	c := func(node, field string) *ir.DataMapping {
		return &ir.DataMapping{Key: "c", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{node, field}}}, Raw: "{{outputs." + node + "." + field + "}}"}
	}
	wf := &ir.Workflow{
		Name:  "partial_backedge",
		Entry: "seed",
		Nodes: map[string]ir.Node{
			"seed": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "seed"}},
			"head": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "head"}},
		},
		Edges: []*ir.Edge{
			{From: "seed", To: "head", With: []*ir.DataMapping{
				c("seed", "zero"),
				{Key: "fail_log", Raw: "gate-findings"},
			}},
			{From: "head", To: "head", LoopName: "spin", With: []*ir.DataMapping{c("head", "c_next")}},
		},
		Schemas: map[string]*ir.Schema{},
		Prompts: map[string]*ir.Prompt{},
		Vars:    map[string]*ir.Var{},
		Loops:   map[string]*ir.Loop{"spin": {Name: "spin", MaxIterations: 3, Entries: map[string]bool{"head": true}, Body: map[string]bool{"head": true}}},
	}
	eng := New(wf, tmpStore(t), newStubExecutor())
	rs := eng.newRunState("run-overlay", nil)
	rs.outputs["seed"] = map[string]any{"zero": int64(0)}
	rs.outputs["head"] = map[string]any{"c_next": int64(1)}
	rs.selectedIncoming["head"] = []store.IncomingEdge{
		{From: "head", To: "head", LoopName: "spin"},
	}

	got := eng.buildNodeInputRS("head", rs.scope())
	if got["c"] != int64(1) {
		t.Fatalf("back-edge overlay of c = %#v, want 1", got["c"])
	}
	if got["fail_log"] != "gate-findings" {
		t.Fatalf("unmapped entry key dropped on re-entry: fail_log=%#v, want %q", got["fail_log"], "gate-findings")
	}
}

func TestSelectedIncoming_StaleRecordFallsBack(t *testing.T) {
	// resume --force against an edited .bot can rehydrate an identity
	// that matches no current edge. Filtering on it would hand the node
	// an empty input (R25212d).
	wf := exclusiveSiblingWorkflow(true)
	eng := New(wf, tmpStore(t), newStubExecutor())
	rs := eng.newRunState("run-stale", nil)
	rs.outputs["copi"] = map[string]any{"has_draft": true}
	rs.outputs["validate"] = map[string]any{"ok": true}
	rs.selectedIncoming["gate"] = []store.IncomingEdge{
		{From: "validate", To: "gate", Condition: "ok"}, // no such guard on the current graph
	}
	got := eng.buildNodeInputRS("gate", rs.scope())
	if got["validated"] != "from-else" {
		t.Fatalf("stale selected incoming dropped mappings: %#v, want last-wins fallback %q", got["validated"], "from-else")
	}
}

func TestMergeJoinIncoming_EmptyUnionLeavesUntracked(t *testing.T) {
	rs := &runState{selectedIncoming: map[string][]store.IncomingEdge{
		"join": {{From: "stale", To: "join"}},
	}}
	mergeJoinIncoming(rs, "join", []*branchResult{
		{err: fmt.Errorf("failed"), selectedIncoming: map[string][]store.IncomingEdge{
			"join": {{From: "a", To: "join"}},
		}},
		{err: fmt.Errorf("failed too")},
	})
	if _, tracked := rs.selectedIncoming["join"]; tracked {
		t.Fatalf("empty union left join tracked: %#v", rs.selectedIncoming["join"])
	}
}
