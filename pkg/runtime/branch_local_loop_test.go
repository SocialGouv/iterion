package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

func TestParallelInvocationRejectsChangedLoopPathOnResume(t *testing.T) {
	parallel := newParallelInvocation("dispatch", "dispatch@outer=1", map[string]string{
		"branch_dispatch_0": "work",
	}, map[string]int{})
	rs := &runState{
		parallel:         parallel,
		loopCounters:     map[string]int{"outer": 2},
		artifactVersions: map[string]int{},
	}
	engine := &Engine{}
	if _, err := engine.ensureParallelInvocation(rs, "dispatch", map[string]string{"branch_dispatch_0": "work"}); err == nil {
		t.Fatal("changed loop path silently discarded durable branch cursors")
	}
}

func TestBranchLocalLoopBudgetGuardDoesNotPriceSiblingSpend(t *testing.T) {
	wf := branchLocalLoopWorkflow()
	wf.Budget = &ir.Budget{MaxTokens: 10_000}
	engine := New(wf, tmpStore(t), newStubExecutor())
	shared := newSharedBudget(wf.Budget, engine.logger)
	parent := &runState{budget: shared, loopBudgetMarks: make(map[string]loopBudgetMark)}
	result := &branchResult{
		outputs:          make(map[string]map[string]any),
		artifacts:        make(map[string]map[string]any),
		artifactVersions: make(map[string]int),
		selectedIncoming: make(map[string][]store.IncomingEdge),
	}
	branch := newBranchRunState(parent, nil, result)
	markLoopBudget(branch, "retry")
	shared.RecordUsage(5_000, 0) // may have been spent by a sibling branch
	if got := engine.loopBudgetShortfall("retry", branch); got != nil {
		t.Fatalf("branch loop priced shared sibling spend: %+v", got)
	}

	trunk := &runState{budget: shared, loopBudgetMarks: make(map[string]loopBudgetMark)}
	shared.Restore(0, 0, 0, 0, 0, 0)
	markLoopBudget(trunk, "retry")
	shared.RecordUsage(5_000, 0)
	if got := engine.loopBudgetShortfall("retry", trunk); got == nil {
		t.Fatal("trunk loop budget guard unexpectedly disabled")
	}
}

func TestBranchLocalLoopCannotUseBudgetExitGrace(t *testing.T) {
	t.Setenv("ITERION_BUDGET_EXIT_GRACE", "0.1")
	wf := branchLocalLoopWorkflow()
	wf.Budget = &ir.Budget{MaxTokens: 10_000}
	engine := New(wf, tmpStore(t), newStubExecutor())
	shared := newSharedBudget(wf.Budget, engine.logger)
	shared.RecordUsage(10_100, 0)
	if _, ok := engine.withinBudgetGrace(&runState{budget: shared, branchLocal: true}); ok {
		t.Fatal("branch-local loop received exit grace without a predictive loop guard")
	}
	if _, ok := engine.withinBudgetGrace(&runState{budget: shared}); !ok {
		t.Fatal("trunk exit grace unexpectedly disabled")
	}
}

func TestRetiredBranchStillRecordsCompletedNodeUsage(t *testing.T) {
	wf := branchLocalLoopWorkflow()
	wf.Budget = &ir.Budget{MaxTokens: 1_000}
	exec := newStubExecutor()
	started := make(chan struct{})
	release := make(chan struct{})
	exec.on("work", func(map[string]any) (map[string]any, error) {
		close(started)
		<-release
		return map[string]any{"_tokens": 7}, nil
	})

	runStore := tmpStore(t)
	const runID = "retired-branch-usage"
	if _, err := runStore.CreateRun(context.Background(), runID, wf.Name, nil); err != nil {
		t.Fatal(err)
	}
	engine := New(wf, runStore, exec)
	rs := engine.newRunState(runID, nil)
	parallel := newParallelInvocation("dispatch", "dispatch@root", map[string]string{"branch": "work"}, nil)
	parallel.captureBase(rs)
	resultCh := make(chan *branchResult, 1)
	go func() {
		resultCh <- engine.execBranch(context.Background(), rs, "branch", wf.Edges[1], map[string]map[string]any{}, map[string]map[string]any{}, "collect", &branchSlot{}, parallel)
	}()
	<-started
	parallel.retire()
	close(release)
	result := <-resultCh
	if result == nil || result.err == nil {
		t.Fatalf("retired branch result = %+v, want cancellation", result)
	}
	tokens, _, _, _, _, _ := rs.budget.Snapshot()
	if tokens != 7 {
		t.Fatalf("recorded tokens = %d, want 7 spent before retirement", tokens)
	}
}

func TestRetiredParallelInvocationRefusesHumanPause(t *testing.T) {
	parallel := newParallelInvocation("dispatch", "dispatch@root", map[string]string{"branch": "gate"}, nil)
	parallel.retire()
	if elected, parked := parallel.beginPause("branch", "gate", &store.BranchCheckpoint{BranchID: "branch"}); elected || parked {
		t.Fatal("retired invocation elected a late human pause")
	}
}

func TestResumedBranchAdvancesAndClearsPendingGateAtomically(t *testing.T) {
	parallel := newParallelInvocation("dispatch", "dispatch@root", map[string]string{"branch": "gate"}, nil)
	parallel.cp.PendingBranchID = "branch"
	parallel.cp.PendingNodeID = "gate"
	parallel.cp.PendingInteractionID = "interaction"
	parallel.cp.PendingInteractionQuestions = map[string]any{"approved": true}
	parallel.resumePending = "branch"
	parallel.resumeBarrier = make(chan struct{})

	advanced := cloneBranchCheckpoint(parallel.cp.Branches["branch"])
	advanced.CurrentNodeID = "work"
	updated, barrier := parallel.updateResumedBranch(advanced)
	if !updated || barrier == nil {
		t.Fatalf("update = %v, barrier = %v; want resumed update and barrier", updated, barrier)
	}
	snapshot := parallel.snapshot()
	if snapshot.Branches["branch"].CurrentNodeID != "work" || snapshot.PendingBranchID != "" || snapshot.PendingNodeID != "" || snapshot.PendingInteractionID != "" {
		t.Fatalf("resumed snapshot = %+v", snapshot)
	}
}

func TestFanOutEachImplicitCollectorAllowsAllDoneLocalLoop(t *testing.T) {
	wf := branchLocalLoopWorkflow()
	wf.Nodes["collect"].(*ir.AgentNode).AwaitMode = ir.AwaitNone
	exec := newStubExecutor()
	exec.on("entry", func(map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{map[string]any{"id": "only"}}}, nil
	})
	workCalls := 0
	exec.on("work", func(input map[string]any) (map[string]any, error) {
		workCalls++
		return map[string]any{"id": input["id"]}, nil
	})
	exec.on("judge", func(input map[string]any) (map[string]any, error) {
		return map[string]any{"id": input["id"], "again": workCalls < 2}, nil
	})
	exec.on("collect", func(map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})

	runStore := tmpStore(t)
	eng := New(wf, runStore, exec)
	if got := eng.findConvergencePoint("dispatch", []*ir.Edge{wf.Edges[1]}); got != "" {
		t.Fatalf("implicit local-loop collector = %q, want terminal convergence", got)
	}
	if err := eng.Run(context.Background(), "implicit-local-loop", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	if workCalls != 2 {
		t.Fatalf("work calls = %d, want 2", workCalls)
	}
}

func TestBranchInvalidHumanAnswerRepromptsWithoutDurableAnswer(t *testing.T) {
	wf := branchFirstHumanLoopWorkflow()
	wf.Nodes["gate"].(*ir.HumanNode).OutputSchema = "gate_answer"
	wf.Schemas["gate_answer"] = &ir.Schema{
		Name: "gate_answer",
		Fields: []*ir.SchemaField{{
			Name: "approved", Type: ir.FieldTypeBool,
		}},
	}
	exec := newStubExecutor()
	exec.on("entry", func(map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{map[string]any{"id": "only"}}}, nil
	})
	exec.on("work", func(input map[string]any) (map[string]any, error) {
		return map[string]any{"id": input["id"]}, nil
	})
	exec.on("judge", func(input map[string]any) (map[string]any, error) {
		return map[string]any{"id": input["id"], "again": false}, nil
	})
	exec.on("collect", func(map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})

	runStore := tmpStore(t)
	const runID = "branch-invalid-human-answer"
	if err := New(wf, runStore, exec, WithOutputValidation(true)).Run(context.Background(), runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("run = %v, want pause", err)
	}
	invalid := map[string]any{"approved": []any{"not", "a", "bool"}}
	if err := New(wf, runStore, exec, WithOutputValidation(true)).Resume(context.Background(), runID, invalid); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("invalid resume = %v, want a new pause", err)
	}
	run, err := runStore.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	branch := run.Checkpoint.Parallel.Branches[run.Checkpoint.Parallel.PendingBranchID]
	if run.Status != store.RunStatusPausedWaitingHuman || run.Checkpoint.PausedNodeID() != "gate" {
		t.Fatalf("invalid answer left run at status=%s checkpoint=%+v", run.Status, run.Checkpoint)
	}
	if branch == nil || len(branch.ResumeAnswers) != 0 {
		t.Fatalf("invalid resume answer remained durable: %+v", branch)
	}
	if err := New(wf, runStore, exec, WithOutputValidation(true)).Resume(context.Background(), runID, map[string]any{"approved": true}); err != nil {
		t.Fatalf("corrected resume: %v", err)
	}
}

func TestBranchEmptyHumanAnswerAdvances(t *testing.T) {
	wf := branchFirstHumanLoopWorkflow()
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
	exec.on("collect", func(map[string]any) (map[string]any, error) {
		return map[string]any{"ok": true}, nil
	})

	runStore := tmpStore(t)
	const runID = "branch-empty-human-answer"
	if err := New(wf, runStore, exec).Run(context.Background(), runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("run = %v, want pause", err)
	}
	if err := New(wf, runStore, exec).Resume(context.Background(), runID, map[string]any{}); err != nil {
		t.Fatalf("empty answer resume = %v, want completed run", err)
	}
	if workCalls != 1 {
		t.Fatalf("work calls = %d, want 1", workCalls)
	}
}

func TestBranchNodeIterationComposesEnclosingAndLocalCounters(t *testing.T) {
	wf := branchLocalLoopWorkflow()
	wf.Loops["outer"] = &ir.Loop{Name: "outer", Body: map[string]bool{"work": true}}
	eng := &Engine{workflow: wf}
	rs := &runState{
		branchLocal:           true,
		enclosingLoopCounters: map[string]int{"outer": 2},
		loopCounters:          map[string]int{"retry": 1},
	}
	iter, path := eng.branchNodeIteration("work", rs)
	if iter != 2 || path != "outer=2;retry=1" {
		t.Fatalf("branch iteration = (%d, %q), want (2, outer=2;retry=1)", iter, path)
	}
	if _, path := eng.branchNodeIteration("work", &runState{}); path != "" {
		t.Fatalf("empty branch iteration path = %q, want omitted", path)
	}
	if got := runStateIterationCounters(rs); got["outer"] != 2 || got["retry"] != 1 {
		t.Fatalf("workspace iteration counters = %#v, want enclosing and local scope", got)
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

// resumeWithinDeadline runs Resume on a fresh engine and fails the test when it
// does not return before the deadline — the shape of a leaked resume barrier.
func resumeWithinDeadline(t *testing.T, wf *ir.Workflow, runStore store.RunStore, exec NodeExecutor, runID string, answers map[string]any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- New(wf, runStore, exec).Resume(ctx, runID, answers) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		t.Fatal("resume hung: answered branch exit did not release the sibling resume barrier")
		return nil
	}
}

type pauseFailStore struct {
	store.RunStore
	err error
}

func (s *pauseFailStore) PauseRun(context.Context, string, *store.Checkpoint) error {
	return s.err
}

type parallelCheckpointCountingStore struct {
	store.RunStore
	mu             sync.Mutex
	parallelWrites int
}

func (s *parallelCheckpointCountingStore) SaveCheckpoint(ctx context.Context, runID string, cp *store.Checkpoint) error {
	if cp != nil && cp.Parallel != nil {
		s.mu.Lock()
		s.parallelWrites++
		s.mu.Unlock()
	}
	return s.RunStore.SaveCheckpoint(ctx, runID, cp)
}

func (s *parallelCheckpointCountingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.parallelWrites
}

func TestFanOutEachLinearBranchCheckpointsOnlyAtRestartBoundaries(t *testing.T) {
	wf := fanOutEachWorkflow(false, ir.AwaitWaitAll, 1)
	wf.Nodes["middle"] = &ir.AgentNode{BaseNode: ir.BaseNode{ID: "middle"}}
	wf.Nodes["last"] = &ir.AgentNode{BaseNode: ir.BaseNode{ID: "last"}}
	for _, edge := range wf.Edges {
		if edge.From == "handle" && edge.To == "collect" {
			edge.To = "middle"
		}
	}
	wf.Edges = append(wf.Edges,
		&ir.Edge{From: "middle", To: "last"},
		&ir.Edge{From: "last", To: "collect"},
	)

	exec := newStubExecutor()
	exec.on("entry", func(map[string]any) (map[string]any, error) {
		return map[string]any{"items": []any{item("only")}}, nil
	})
	base := tmpStore(t)
	runStore := &parallelCheckpointCountingStore{RunStore: base}
	if err := New(wf, runStore, exec).Run(context.Background(), "linear-branch-checkpoint-boundaries", nil); err != nil {
		t.Fatal(err)
	}
	// One write initializes the invocation and one records branch completion.
	// The three linear node boundaries must not add full ParallelCheckpoint
	// rewrites under the invocation-wide save mutex.
	if got := runStore.count(); got != 2 {
		t.Fatalf("parallel checkpoint writes = %d, want 2 (initialization + completion)", got)
	}
}

func TestParallelCheckpointPrunesDurableArtifactAllocationKeys(t *testing.T) {
	const branchID = "branch_dispatch_0"
	parallel := newParallelInvocation("dispatch", "dispatch@root", map[string]string{branchID: "work"}, nil)
	if got := parallel.artifactVersion("work", branchID+"/work/retry=0", 0); got != 0 {
		t.Fatalf("first artifact version = %d, want 0", got)
	}
	if got := parallel.artifactVersion("work", branchID+"/work/retry=1", 0); got != 1 {
		t.Fatalf("second artifact version = %d, want 1", got)
	}
	if !parallel.updateBranch(&store.BranchCheckpoint{BranchID: branchID, CurrentNodeID: "next"}) {
		t.Fatal("branch cursor update was rejected")
	}
	snapshot := parallel.snapshot()
	if len(snapshot.ArtifactAllocations) != 0 {
		t.Fatalf("durable artifact allocations = %#v, want pruned after cursor advance", snapshot.ArtifactAllocations)
	}
	if got := parallel.artifactVersion("work", branchID+"/work/retry=2", 0); got != 2 {
		t.Fatalf("version after pruning = %d, want monotonic version 2", got)
	}
}

func TestFanOutPausePersistenceFailureIsNotReportedAsPaused(t *testing.T) {
	wf := branchFirstHumanLoopWorkflow()
	exec := newStubExecutor()
	exec.on("entry", func(map[string]any) (map[string]any, error) {
		return map[string]any{
			"items": []any{map[string]any{"id": "first"}, map[string]any{"id": "second"}},
		}, nil
	})

	base := tmpStore(t)
	pauseErr := errors.New("pause store unavailable")
	runStore := &pauseFailStore{RunStore: base, err: pauseErr}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := New(wf, runStore, exec).Run(ctx, "branch-pause-store-failure", nil)
	if err == nil || errors.Is(err, ErrRunPaused) || !strings.Contains(err.Error(), pauseErr.Error()) {
		t.Fatalf("run error = %v, want visible pause persistence failure (not ErrRunPaused)", err)
	}
	run, loadErr := base.LoadRun(context.Background(), "branch-pause-store-failure")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if run.Status == store.RunStatusPausedWaitingHuman {
		t.Fatalf("status = %s after PauseRun failure, want a non-paused status", run.Status)
	}
}

func TestFanOutEachHumanGateEventsStayPairedAcrossSiblingResumes(t *testing.T) {
	wf := branchFirstHumanLoopWorkflow()
	exec := newStubExecutor()
	exec.on("entry", func(map[string]any) (map[string]any, error) {
		return map[string]any{
			"items": []any{
				map[string]any{"id": "first"},
				map[string]any{"id": "second"},
				map[string]any{"id": "third"},
			},
		}, nil
	})
	exec.on("work", func(input map[string]any) (map[string]any, error) {
		return map[string]any{"id": input["id"]}, nil
	})
	exec.on("judge", func(input map[string]any) (map[string]any, error) {
		return map[string]any{"id": input["id"], "again": false}, nil
	})

	runStore := tmpStore(t)
	runID := "branch-gate-paired-events"
	err := New(wf, runStore, exec).Run(context.Background(), runID, nil)
	if !errors.Is(err, ErrRunPaused) {
		t.Fatalf("run = %v, want first branch pause", err)
	}
	for resumes := 0; resumes < 5 && errors.Is(err, ErrRunPaused); resumes++ {
		err = resumeWithinDeadline(t, wf, runStore, exec, runID, map[string]any{"approved": true})
	}
	if err != nil {
		t.Fatalf("final resume = %v, want all three branches complete", err)
	}

	events, err := runStore.LoadEvents(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	type counts struct{ started, finished, requested int }
	byBranch := make(map[string]*counts)
	for _, event := range events {
		if event.NodeID != "gate" || event.BranchID == "" {
			continue
		}
		if byBranch[event.BranchID] == nil {
			byBranch[event.BranchID] = &counts{}
		}
		switch event.Type {
		case store.EventNodeStarted:
			byBranch[event.BranchID].started++
		case store.EventNodeFinished:
			byBranch[event.BranchID].finished++
		case store.EventHumanInputRequested:
			byBranch[event.BranchID].requested++
		}
	}
	if len(byBranch) != 3 {
		t.Fatalf("gate event branches = %d (%v), want 3", len(byBranch), byBranch)
	}
	for branchID, got := range byBranch {
		if got.started != 1 || got.finished != 1 || got.requested != 1 {
			t.Errorf("%s gate events = %+v, want one started/finished/requested", branchID, *got)
		}
	}
}

// A plain (non-pause, non-budget) error in the answered branch — here an
// answer that satisfies no outgoing edge — must not wedge the fan-out: under
// best_effort nothing cancels the siblings, so they only leave the resume
// barrier if the answered branch's exit releases it. The failed branch also
// hands its gate back (no pending identity, no consumed answers), so a sibling
// can elect its own gate and the next resume re-asks the failed branch's
// question instead of replaying the payload that already failed.
func TestFanOutEachPendingBranchErrorReleasesResumeBarrier(t *testing.T) {
	wf := branchFirstHumanLoopWorkflow()
	wf.Nodes["collect"].(*ir.AgentNode).AwaitMode = ir.AwaitBestEffort
	for _, edge := range wf.Edges {
		if edge.From == "gate" && edge.To == "work" {
			edge.Condition = "approved"
		}
	}
	exec := newStubExecutor()
	exec.on("entry", func(map[string]any) (map[string]any, error) {
		return map[string]any{
			"items": []any{map[string]any{"id": "first"}, map[string]any{"id": "second"}},
		}, nil
	})
	var mu sync.Mutex
	workCalls := map[string]int{}
	exec.on("work", func(input map[string]any) (map[string]any, error) {
		mu.Lock()
		workCalls[input["id"].(string)]++
		mu.Unlock()
		return map[string]any{"id": input["id"]}, nil
	})
	exec.on("judge", func(input map[string]any) (map[string]any, error) {
		return map[string]any{"id": input["id"], "again": false}, nil
	})
	exec.on("collect", func(map[string]any) (map[string]any, error) { return map[string]any{"ok": true}, nil })

	runStore := tmpStore(t)
	runID := "branch-resume-plain-error"
	if err := New(wf, runStore, exec).Run(context.Background(), runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("run = %v, want pause", err)
	}
	run, err := runStore.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	failedBranch := run.Checkpoint.Parallel.PendingBranchID
	if failedBranch == "" {
		t.Fatalf("checkpoint = %+v, want an elected pending branch", run.Checkpoint.Parallel)
	}

	// The unsatisfiable answer fails the answered branch with a plain error;
	// its sibling must still run to its own gate and pause the run.
	if err := resumeWithinDeadline(t, wf, runStore, exec, runID, map[string]any{"approved": false}); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("resume after unsatisfiable answer = %v, want the sibling's pause", err)
	}
	run, err = runStore.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunStatusPausedWaitingHuman {
		t.Fatalf("status = %s, want paused_waiting_human", run.Status)
	}
	ps := run.Checkpoint.Parallel
	if ps == nil || ps.PendingBranchID == "" || ps.PendingBranchID == failedBranch {
		t.Fatalf("parallel checkpoint = %+v, want the sibling elected, not %s", ps, failedBranch)
	}
	if run.Checkpoint.InteractionID != ps.PendingInteractionID {
		t.Fatalf("checkpoint interaction %q != pending %q", run.Checkpoint.InteractionID, ps.PendingInteractionID)
	}
	failed := ps.Branches[failedBranch]
	if failed == nil || failed.CurrentNodeID != "gate" || failed.Completed || failed.ResumeAnswered || len(failed.ResumeAnswers) != 0 {
		t.Fatalf("failed branch checkpoint = %+v, want parked at gate without consumed answers", failed)
	}

	// Answer the sibling: it advances, and the failed branch re-asks its gate.
	if err := resumeWithinDeadline(t, wf, runStore, exec, runID, map[string]any{"approved": true}); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("second resume = %v, want the failed branch to re-ask", err)
	}
	run, err = runStore.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Checkpoint.Parallel.PendingBranchID != failedBranch {
		t.Fatalf("pending = %q, want the re-asked branch %s", run.Checkpoint.Parallel.PendingBranchID, failedBranch)
	}
	if err := resumeWithinDeadline(t, wf, runStore, exec, runID, map[string]any{"approved": true}); err != nil {
		t.Fatalf("final resume = %v, want completion", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if workCalls["first"] == 0 || workCalls["second"] == 0 {
		t.Fatalf("work calls = %#v, want both items to complete", workCalls)
	}
}

// The fan_out_all launcher has its own post-branch policy; cover it too.
func TestFanOutAllPendingBranchErrorReleasesResumeBarrier(t *testing.T) {
	wf := &ir.Workflow{
		Name:  "fanout_gate_error",
		Entry: "entry",
		Nodes: map[string]ir.Node{
			"entry":   &ir.AgentNode{BaseNode: ir.BaseNode{ID: "entry"}},
			"router":  &ir.RouterNode{BaseNode: ir.BaseNode{ID: "router"}, RouterMode: ir.RouterFanOutAll},
			"gate_a":  &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate_a"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}},
			"gate_b":  &ir.HumanNode{BaseNode: ir.BaseNode{ID: "gate_b"}, InteractionFields: ir.InteractionFields{Interaction: ir.InteractionHuman}},
			"work_a":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work_a"}},
			"work_b":  &ir.AgentNode{BaseNode: ir.BaseNode{ID: "work_b"}},
			"collect": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "collect"}, AwaitMode: ir.AwaitBestEffort},
			"done":    &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}},
		},
		Edges: []*ir.Edge{
			{From: "entry", To: "router"},
			{From: "router", To: "gate_a"},
			{From: "router", To: "gate_b"},
			{From: "gate_a", To: "work_a", Condition: "approved"},
			{From: "gate_b", To: "work_b", Condition: "approved"},
			{From: "work_a", To: "collect"},
			{From: "work_b", To: "collect"},
			{From: "collect", To: "done"},
		},
		Schemas:   map[string]*ir.Schema{},
		Prompts:   map[string]*ir.Prompt{},
		Vars:      map[string]*ir.Var{},
		Loops:     map[string]*ir.Loop{},
		Foreaches: map[string]*ir.Foreach{},
	}
	exec := newStubExecutor()
	exec.on("entry", func(map[string]any) (map[string]any, error) { return map[string]any{"ok": true}, nil })
	var mu sync.Mutex
	workCalls := map[string]int{}
	work := func(id string) func(map[string]any) (map[string]any, error) {
		return func(map[string]any) (map[string]any, error) {
			mu.Lock()
			workCalls[id]++
			mu.Unlock()
			return map[string]any{"id": id}, nil
		}
	}
	exec.on("work_a", work("a"))
	exec.on("work_b", work("b"))
	exec.on("collect", func(map[string]any) (map[string]any, error) { return map[string]any{"ok": true}, nil })

	runStore := tmpStore(t)
	runID := "fanout-all-resume-plain-error"
	if err := New(wf, runStore, exec).Run(context.Background(), runID, nil); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("run = %v, want pause", err)
	}
	run, err := runStore.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	failedBranch := run.Checkpoint.Parallel.PendingBranchID
	if failedBranch == "" {
		t.Fatalf("checkpoint = %+v, want an elected pending branch", run.Checkpoint.Parallel)
	}
	if err := resumeWithinDeadline(t, wf, runStore, exec, runID, map[string]any{"approved": false}); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("resume after unsatisfiable answer = %v, want the sibling's pause", err)
	}
	run, err = runStore.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	ps := run.Checkpoint.Parallel
	if run.Status != store.RunStatusPausedWaitingHuman || ps == nil || ps.PendingBranchID == "" || ps.PendingBranchID == failedBranch {
		t.Fatalf("status=%s parallel=%+v, want the sibling elected, not %s", run.Status, ps, failedBranch)
	}
	if failed := ps.Branches[failedBranch]; failed == nil || failed.Completed || failed.ResumeAnswered || len(failed.ResumeAnswers) != 0 {
		t.Fatalf("failed branch checkpoint = %+v, want parked without consumed answers", failed)
	}
	if err := resumeWithinDeadline(t, wf, runStore, exec, runID, map[string]any{"approved": true}); !errors.Is(err, ErrRunPaused) {
		t.Fatalf("second resume = %v, want the failed branch to re-ask", err)
	}
	if err := resumeWithinDeadline(t, wf, runStore, exec, runID, map[string]any{"approved": true}); err != nil {
		t.Fatalf("final resume = %v, want completion", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if workCalls["a"] == 0 || workCalls["b"] == 0 {
		t.Fatalf("work calls = %#v, want both branches to complete", workCalls)
	}
}
