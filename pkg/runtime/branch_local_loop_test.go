package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

func branchLocalLoopWorkflow() *ir.Workflow {
	dispatch := &ir.RouterNode{
		BaseNode:    ir.BaseNode{ID: "dispatch"},
		RouterMode:  ir.RouterFanOutEach,
		Over:        "{{outputs.entry.items}}",
		OverRefs:    []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"entry", "items"}, Raw: "{{outputs.entry.items}}"}},
		ItemBinding: "item",
	}
	idFrom := func(node string) []*ir.DataMapping {
		raw := "{{outputs." + node + ".id}}"
		return []*ir.DataMapping{{Key: "id", Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{node, "id"}, Raw: raw}}, Raw: raw}}
	}
	wf := &ir.Workflow{
		Name:  "branch_local_loop",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"dispatch": dispatch,
			"work":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work"}},
			"judge":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "judge"}},
			"collect":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "collect"}, AwaitMode: ir.AwaitWaitAll},
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":     &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "dispatch"},
			{
				From: "dispatch",
				To:   "work",
				With: []*ir.DataMapping{{
					Key:  "id",
					Refs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"dispatch", "item", "id"}, Raw: "{{outputs.dispatch.item.id}}"}},
					Raw:  "{{outputs.dispatch.item.id}}",
				}},
			},
			{From: "work", To: "judge", With: idFrom("work")},
			{From: "judge", To: "work", LoopName: "retry", Condition: "again", With: idFrom("judge")},
			{From: "judge", To: "collect", IsElse: true},
			{From: "collect", To: "done"},
		},
		Loops: map[string]*ir.Loop{
			"retry": {Name: "retry", MaxIterations: 5, Entries: map[string]bool{"work": true}, Body: map[string]bool{"work": true, "judge": true}},
		},
		Foreaches: map[string]*ir.Foreach{},
		Schemas:   map[string]*ir.Schema{},
		Prompts:   map[string]*ir.Prompt{},
		Vars:      map[string]*ir.Var{},
	}
	return wf
}

func branchFirstHumanLoopWorkflow() *ir.Workflow {
	wf := branchLocalLoopWorkflow()
	wf.Nodes["gate"] = &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}}
	for _, edge := range wf.Edges {
		if edge.From == "dispatch" && edge.To == "work" {
			edge.To = "gate"
		}
	}
	wf.Edges = append(wf.Edges, &ir.Edge{From: "gate", To: "work", With: []*ir.DataMapping{{
		Key: "id",
		Refs: []*ir.Ref{{
			Kind: ir.RefOutputs,
			Path: []string{"dispatch", "item", "id"},
			Raw:  "{{outputs.dispatch.item.id}}",
		}},
		Raw: "{{outputs.dispatch.item.id}}",
	}}})
	return wf
}

func TestFanOutEachResumeBeforeFirstLocalIterationNormalizesDurableMaps(t *testing.T) {
	wf := branchFirstHumanLoopWorkflow()
	wf.Nodes["work"].(*ir.AgentNode).Publish = "work_artifact"
	exec := newStubExecutor()
	exec.on("entry", func(map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{map[string]any{"id": "only"}}}, nil
	})
	var workCalls int
	exec.on("work", func(input map[string]any) (map[string]any, error) {
		workCalls++
		return map[string]any{"id": input["id"]}, nil
	})
	exec.on("judge", func(input map[string]any) (map[string]any, error) {
		return map[string]any{"id": input["id"], "again": workCalls < 2}, nil
	})
	exec.on("collect", func(map[string]any) (map[string]any, error) { return map[string]any{"ok": true}, nil })

	runStore := tmpStore(t)
	runID := "branch-resume-before-first-loop"
	if err := New(wf, runStore, exec).Run(context.Background(), runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("run = %v, want pause", err)
	}
	run, err := runStore.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	branch := run.Checkpoint.Parallel.Branches["branch_dispatch_0"]
	if branch == nil || branch.CurrentNodeID != "gate" {
		t.Fatalf("branch checkpoint = %+v, want gate", branch)
	}
	// Empty omitempty maps round-trip as nil in the file store. Resume must
	// normalize them before the first counter/allocation write.
	if branch.LoopCounters != nil || run.Checkpoint.Parallel.ArtifactAllocations != nil {
		t.Fatalf("expected omitted maps after round-trip, got counters=%#v allocations=%#v", branch.LoopCounters, run.Checkpoint.Parallel.ArtifactAllocations)
	}

	if err := New(wf, runStore, exec).Resume(context.Background(), runID, map[string]any{"approved": true}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if workCalls != 2 {
		t.Fatalf("work calls = %d, want 2", workCalls)
	}
	for version := 0; version < 2; version++ {
		if _, err := runStore.LoadArtifact(context.Background(), runID, "work", version); err != nil {
			t.Fatalf("artifact version %d: %v", version, err)
		}
	}
}

func TestInitBranchResultKeepsSelectedIncomingWritable(t *testing.T) {
	rs := &runState{artifactVersions: map[string]int{}}
	result := initBranchResult(rs, "branch", &store.BranchCheckpoint{BranchID: "branch"})
	edge := &ir.Edge{From: "router", To: "work"}
	recordIncoming(result.selectedIncoming, edge, true)
	if got := result.selectedIncoming["work"]; len(got) != 1 || got[0].From != "router" {
		t.Fatalf("selected incoming = %#v, want writable start-edge record", result.selectedIncoming)
	}
}

func TestParallelInvocationRejectsChangedBranchSetOnResume(t *testing.T) {
	parallel := newParallelInvocation("dispatch", "dispatch@root", map[string]string{
		"branch_dispatch_0": "work",
		"branch_dispatch_1": "work",
	}, map[string]int{})
	rs := &runState{
		parallel:         parallel,
		loopCounters:     map[string]int{},
		artifactVersions: map[string]int{},
	}
	engine := &Engine{}
	if _, err := engine.ensureParallelInvocation(rs, "dispatch", map[string]string{"branch_dispatch_0": "work"}); err == nil {
		t.Fatal("changed branch set resumed silently, want a rewind-required error")
	}
}

func TestFanOutEachPendingBranchPanicReleasesResumeBarrier(t *testing.T) {
	wf := branchFirstHumanLoopWorkflow()
	exec := newStubExecutor()
	exec.on("entry", func(map[string]any) (map[string]any, error) {
		return map[string]any{
			"items": []any{map[string]any{"id": "first"}, map[string]any{"id": "second"}},
		}, nil
	})
	exec.on("work", func(map[string]any) (map[string]any, error) {
		panic("post-resume boom")
	})

	runStore := tmpStore(t)
	runID := "branch-resume-panic"
	if err := New(wf, runStore, exec).Run(context.Background(), runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("run = %v, want pause", err)
	}
	run, err := runStore.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, branchID := range []string{"branch_dispatch_0", "branch_dispatch_1"} {
		branch := run.Checkpoint.Parallel.Branches[branchID]
		if branch == nil || branch.CurrentNodeID != "gate" {
			t.Fatalf("branch %s checkpoint = %+v, want durable gate cursor", branchID, branch)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- New(wf, runStore, exec).Resume(ctx, runID, map[string]any{"approved": true})
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("resume unexpectedly succeeded after branch panic")
		}
	case <-ctx.Done():
		t.Fatal("resume hung: panic did not release sibling resume barrier")
	}
}

func TestFanOutEachBranchLocalLoopHumanResumeKeepsScope(t *testing.T) {
	wf := branchLocalLoopWorkflow()
	wf.Nodes["gate"] = &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}}
	for _, edge := range wf.Edges {
		if edge.From == "judge" && edge.To == "collect" {
			edge.To = "gate"
		}
	}
	wf.Edges = append(wf.Edges, &ir.Edge{From: "gate", To: "collect"})

	exec := newStubExecutor()
	exec.on("entry", func(map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{map[string]any{"id": "only"}}}, nil
	})
	var workCalls int
	exec.on("work", func(input map[string]any) (map[string]any, error) {
		workCalls++
		return map[string]any{"id": input["id"]}, nil
	})
	exec.on("judge", func(input map[string]any) (map[string]any, error) {
		return map[string]any{"id": input["id"], "again": workCalls < 2}, nil
	})
	exec.on("collect", func(map[string]any) (map[string]any, error) { return map[string]any{"ok": true}, nil })

	runStore := tmpStore(t)
	eng := New(wf, runStore, exec)
	if err := eng.Run(context.Background(), "branch-local-human", nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("first run error = %v, want ErrRunPaused", err)
	}
	run, err := runStore.LoadRun(context.Background(), "branch-local-human")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunStatusPausedWaitingHuman || run.Checkpoint == nil || run.Checkpoint.Parallel == nil {
		t.Fatalf("run did not persist parallel pause: status=%s checkpoint=%+v", run.Status, run.Checkpoint)
	}
	branch := run.Checkpoint.Parallel.Branches["branch_dispatch_0"]
	if branch == nil || branch.LoopCounters["retry"] != 1 || branch.CurrentNodeID != "gate" {
		t.Fatalf("branch checkpoint = %+v, want retry=1 at gate", branch)
	}

	// A fresh engine instance simulates a runner restart: only the durable
	// checkpoint may carry the branch's loop scope across this boundary.
	eng = New(wf, runStore, exec)
	if err := eng.Resume(context.Background(), "branch-local-human", map[string]any{"approved": true}); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if workCalls != 2 {
		t.Fatalf("work calls after resume = %d, want 2 (completed loop iterations must not replay)", workCalls)
	}
}

func TestFanOutEachSiblingHumanGatesResumeOneScopeAtATime(t *testing.T) {
	wf := branchLocalLoopWorkflow()
	wf.Nodes["gate"] = &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}}
	for _, edge := range wf.Edges {
		if edge.From == "judge" && edge.To == "collect" {
			edge.To = "gate"
		}
	}
	wf.Edges = append(wf.Edges, &ir.Edge{From: "gate", To: "collect"})

	exec := newStubExecutor()
	exec.on("entry", func(map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{map[string]any{"id": "first"}, map[string]any{"id": "second"}}}, nil
	})
	var mu sync.Mutex
	workCalls := map[string]int{}
	exec.on("work", func(input map[string]any) (map[string]any, error) {
		id := input["id"].(string)
		mu.Lock()
		workCalls[id]++
		mu.Unlock()
		return map[string]any{"id": id}, nil
	})
	exec.on("judge", func(input map[string]any) (map[string]any, error) {
		return map[string]any{"id": input["id"], "again": false}, nil
	})
	exec.on("collect", func(map[string]any) (map[string]any, error) { return map[string]any{"ok": true}, nil })

	runStore := tmpStore(t)
	runID := "branch-local-sibling-humans"
	eng := New(wf, runStore, exec)
	if err := eng.Run(context.Background(), runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("first run = %v, want pause", err)
	}
	eng = New(wf, runStore, exec)
	if err := eng.Resume(context.Background(), runID, map[string]any{"approved": true}); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("first resume = %v, want sibling pause", err)
	}
	eng = New(wf, runStore, exec)
	if err := eng.Resume(context.Background(), runID, map[string]any{"approved": true}); err != nil {
		t.Fatalf("second resume: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if workCalls["first"] != 1 || workCalls["second"] != 1 {
		t.Fatalf("work calls = %#v, completed siblings were replayed", workCalls)
	}
}

func TestFanOutEachBranchCanPauseAtSequentialHumanGates(t *testing.T) {
	wf := branchLocalLoopWorkflow()
	wf.Nodes["gate_one"] = &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate_one"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}}
	wf.Nodes["gate_two"] = &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate_two"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}}
	for _, edge := range wf.Edges {
		if edge.From == "judge" && edge.To == "collect" {
			edge.To = "gate_one"
		}
	}
	wf.Edges = append(wf.Edges,
		&ir.Edge{From: "gate_one", To: "gate_two"},
		&ir.Edge{From: "gate_two", To: "collect"},
	)

	exec := newStubExecutor()
	exec.on("entry", func(map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{map[string]any{"id": "only"}}}, nil
	})
	var workCalls int
	exec.on("work", func(input map[string]any) (map[string]any, error) {
		workCalls++
		return map[string]any{"id": input["id"]}, nil
	})
	exec.on("judge", func(input map[string]any) (map[string]any, error) {
		return map[string]any{"id": input["id"], "again": false}, nil
	})
	exec.on("collect", func(map[string]any) (map[string]any, error) { return map[string]any{"ok": true}, nil })

	runStore := tmpStore(t)
	runID := "branch-local-sequential-humans"
	eng := New(wf, runStore, exec)
	if err := eng.Run(context.Background(), runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("first run = %v, want first pause", err)
	}
	eng = New(wf, runStore, exec)
	if err := eng.Resume(context.Background(), runID, map[string]any{"approved": true}); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("first resume = %v, want second pause", err)
	}
	run, err := runStore.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Checkpoint == nil || run.Checkpoint.Parallel == nil || run.Checkpoint.Parallel.PendingNodeID != "gate_two" {
		t.Fatalf("checkpoint after first resume = %+v, want gate_two", run.Checkpoint)
	}
	eng = New(wf, runStore, exec)
	if err := eng.Resume(context.Background(), runID, map[string]any{"approved": true}); err != nil {
		t.Fatalf("second resume: %v", err)
	}
	if workCalls != 1 {
		t.Fatalf("work calls = %d, want 1", workCalls)
	}
}

func TestFanOutEachBranchLocalLoopsKeepIndependentCounters(t *testing.T) {
	wf := branchLocalLoopWorkflow()
	wf.Nodes["work"].(*ir.AgentNode).Publish = "work_artifact"
	exec := newStubExecutor()
	exec.on("entry", func(map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{map[string]any{"id": "short"}, map[string]any{"id": "long"}}}, nil
	})

	var mu sync.Mutex
	calls := map[string]int{}
	exec.on("work", func(input map[string]any) (map[string]any, error) {
		id, _ := input["id"].(string)
		mu.Lock()
		calls[id]++
		mu.Unlock()
		return map[string]any{"id": id}, nil
	})
	exec.on("judge", func(input map[string]any) (map[string]any, error) {
		id, _ := input["id"].(string)
		mu.Lock()
		n := calls[id]
		mu.Unlock()
		want := 1
		if id == "long" {
			want = 3
		}
		return map[string]any{"id": id, "again": n < want}, nil
	})
	exec.on("collect", func(map[string]any) (map[string]any, error) { return map[string]any{"ok": true}, nil })

	runStore := tmpStore(t)
	eng := New(wf, runStore, exec)
	if err := eng.Run(context.Background(), "branch-local-independent", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls["short"] != 1 || calls["long"] != 3 {
		t.Fatalf("work calls = %#v, want short=1 long=3", calls)
	}
	for version := 0; version < 4; version++ {
		if _, err := runStore.LoadArtifact(context.Background(), "branch-local-independent", "work", version); err != nil {
			t.Fatalf("artifact version %d: %v", version, err)
		}
	}
}

func TestFanOutAllBranchLocalLoopRunsBeforeWaitAll(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "fan_out_all_branch_loop",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"dispatch": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "dispatch"}, RouterMode: ir.RouterFanOutAll},
			"retrying": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "retrying"}},
			"once":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "once"}},
			"collect":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "collect"}, AwaitMode: ir.AwaitWaitAll},
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":     &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "dispatch"},
			{From: "dispatch", To: "retrying"},
			{From: "dispatch", To: "once"},
			{From: "retrying", To: "retrying", LoopName: "local", Condition: "again"},
			{From: "retrying", To: "collect", IsElse: true},
			{From: "once", To: "collect"},
			{From: "collect", To: "done"},
		},
		Loops: map[string]*ir.Loop{
			"local": {Name: "local", MaxIterations: 5, Entries: map[string]bool{"retrying": true}, Body: map[string]bool{"retrying": true}},
		},
		Foreaches: map[string]*ir.Foreach{}, Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{}, Vars: map[string]*ir.Var{},
	}
	exec := newStubExecutor()
	exec.on("entry", func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
	var retryCalls, onceCalls int
	exec.on("retrying", func(map[string]any) (map[string]any, error) {
		retryCalls++
		return map[string]any{"again": retryCalls < 3}, nil
	})
	exec.on("once", func(map[string]any) (map[string]any, error) {
		onceCalls++
		return map[string]any{}, nil
	})
	exec.on("collect", func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
	if err := New(wf, tmpStore(t), exec).Run(context.Background(), "fan-out-all-local-loop", nil); err != nil {
		t.Fatal(err)
	}
	if retryCalls != 3 || onceCalls != 1 {
		t.Fatalf("calls retrying=%d once=%d, want 3 and 1", retryCalls, onceCalls)
	}
}

func TestFanOutEachBranchLocalForeachUsesPrivateIndex(t *testing.T) {
	eachRef := &ir.Ref{Kind: ir.RefEach, Path: []string{"scan", "item"}, Raw: "{{each.scan.item}}"}
	eachMapping := []*ir.DataMapping{{Key: "value", Refs: []*ir.Ref{eachRef}, Raw: eachRef.Raw}}
	wf := &ir.Workflow{
		Name:  "branch_foreach",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":    &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"dispatch": &ir.RouterNode{BaseNode: ir.BaseNode{ID: "dispatch"}, RouterMode: ir.RouterFanOutEach, Over: "{{outputs.entry.items}}", OverRefs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"entry", "items"}}}, ItemBinding: "item"},
			"work":     &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work"}},
			"collect":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "collect"}, AwaitMode: ir.AwaitWaitAll},
			"done":     &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
			"fail":     &ir.FailNode{BaseNode: ir.BaseNode{ID: "fail"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "dispatch"},
			{From: "dispatch", To: "work", With: eachMapping},
			{From: "work", To: "work", ForeachName: "scan", With: eachMapping},
			{From: "work", To: "collect"},
			{From: "collect", To: "done"},
		},
		Loops: map[string]*ir.Loop{},
		Foreaches: map[string]*ir.Foreach{
			"scan": {Name: "scan", Item: "part", CollectionRaw: "{{outputs.entry.parts}}", CollectionRefs: []*ir.Ref{{Kind: ir.RefOutputs, Path: []string{"entry", "parts"}}}},
		},
		Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{}, Vars: map[string]*ir.Var{},
	}
	exec := newStubExecutor()
	exec.on("entry", func(map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{"one"}, "parts": []any{"a", "b", "c"}}, nil
	})
	var values []string
	exec.on("work", func(input map[string]any) (map[string]any, error) {
		values = append(values, input["value"].(string))
		return map[string]any{}, nil
	})
	exec.on("collect", func(map[string]any) (map[string]any, error) { return map[string]any{}, nil })
	if err := New(wf, tmpStore(t), exec).Run(context.Background(), "branch-local-foreach", nil); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(values); got != "[a b c]" {
		t.Fatalf("foreach values = %s, want [a b c]", got)
	}
}
