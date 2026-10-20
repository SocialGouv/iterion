package runtime

import (
	"context"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

type runnerHomeCall struct {
	nodeID    string
	homeScope string
	runID     string
}

type runnerHomeBackend struct {
	mu    sync.Mutex
	calls []runnerHomeCall
}

func (b *runnerHomeBackend) Execute(ctx context.Context, task delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	b.calls = append(b.calls, runnerHomeCall{
		nodeID:    task.NodeID,
		homeScope: task.HomeScope,
		runID:     model.RunIDFromContext(ctx),
	})
	b.mu.Unlock()
	if task.NodeID == "entry" {
		return delegate.Result{Output: map[string]any{"items": []any{item("item-a"), item("item-b")}}}, nil
	}
	return delegate.Result{Output: map[string]any{"ok": true}}, nil
}

func TestFanOutEachRunnerHomeScopeSeparatesItemsNotNodes(t *testing.T) {
	wf := fanOutEachWorkflow(false, ir.AwaitWaitAll, 0)
	wf.Nodes["handle"] = &ir.AgentNode{BaseNode: ir.BaseNode{ID: "handle"}, LLMFields: ir.LLMFields{Readonly: true}}
	wf.Nodes["middle"] = &ir.AgentNode{BaseNode: ir.BaseNode{ID: "middle"}, LLMFields: ir.LLMFields{Readonly: true}}
	for _, edge := range wf.Edges {
		if edge.From == "handle" && edge.To == "collect" {
			edge.From = "middle"
		}
	}
	wf.Edges = append(wf.Edges, &ir.Edge{From: "handle", To: "middle"})

	backend := &runnerHomeBackend{}
	backends := delegate.NewRegistry()
	backends.Register("runner-home", backend)
	exec := model.NewClawExecutor(model.NewRegistry(), wf,
		model.WithBackendRegistry(backends),
		model.WithDefaultBackend("runner-home"),
		model.WithWorkDir(t.TempDir()),
	)
	defer exec.Close()

	const runID = "run-fanout-home-scope"
	if err := New(wf, tmpStore(t), exec).Run(context.Background(), runID, nil); err != nil {
		t.Fatalf("run: %v", err)
	}

	backend.mu.Lock()
	calls := append([]runnerHomeCall(nil), backend.calls...)
	backend.mu.Unlock()
	byNode := map[string][]runnerHomeCall{}
	for _, call := range calls {
		byNode[call.nodeID] = append(byNode[call.nodeID], call)
	}
	trunkScope := model.RunnerHomeScopeForRun(runID)
	for _, nodeID := range []string{"entry", "collect"} {
		if len(byNode[nodeID]) != 1 {
			t.Fatalf("%s calls = %d, want one", nodeID, len(byNode[nodeID]))
		}
		if byNode[nodeID][0].homeScope != trunkScope {
			t.Errorf("%s HomeScope = %q, want the trunk scope %q", nodeID, byNode[nodeID][0].homeScope, trunkScope)
		}
		if byNode[nodeID][0].runID != runID {
			t.Errorf("%s RunID = %q, want %q", nodeID, byNode[nodeID][0].runID, runID)
		}
	}

	wantBranchScopes := map[string]bool{
		model.RunnerHomeScopeForBranch(runID, "branch_dispatch_0"): true,
		model.RunnerHomeScopeForBranch(runID, "branch_dispatch_1"): true,
	}
	branchScopeNodes := map[string]map[string]bool{}
	for _, nodeID := range []string{"handle", "middle"} {
		if len(byNode[nodeID]) != 2 {
			t.Fatalf("%s calls = %d, want one per item", nodeID, len(byNode[nodeID]))
		}
		for _, call := range byNode[nodeID] {
			if call.runID != "" {
				t.Errorf("%s carries RunID %q inside a fan-out branch — HomeScope must not resurrect the session identity", nodeID, call.runID)
			}
			if branchScopeNodes[call.homeScope] == nil {
				branchScopeNodes[call.homeScope] = map[string]bool{}
			}
			branchScopeNodes[call.homeScope][nodeID] = true
		}
	}
	if len(branchScopeNodes) != len(wantBranchScopes) {
		t.Fatalf("branch HomeScopes = %v, want one per item %v", branchScopeNodes, wantBranchScopes)
	}
	for scope := range wantBranchScopes {
		if scope == trunkScope {
			t.Errorf("a branch uses the trunk HomeScope %q", scope)
		}
		if !branchScopeNodes[scope]["handle"] || !branchScopeNodes[scope]["middle"] {
			t.Errorf("HomeScope %q is not shared by both nodes of its item: %v", scope, branchScopeNodes[scope])
		}
	}
}
