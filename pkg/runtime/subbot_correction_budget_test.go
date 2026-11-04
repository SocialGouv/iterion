package runtime

import (
	"context"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

func TestFreshSubbotRetainsCorrectionAllowance(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	parent := validationWorkflow()
	parent.Nodes["my_agent"] = &ir.SubbotNode{BaseNode: ir.BaseNode{ID: "my_agent"}, Source: "child.bot"}
	child := validationWorkflow()
	child.Nodes["my_agent"].(*ir.AgentNode).Backend = delegate.BackendClaudeCode
	child.Schemas["MySchema"].Fields[0].EnumValues = []string{"pass"}
	st := tmpStore(t)
	calls, children := 0, 0
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, adverseBackendFunc(func(context.Context, delegate.Task) (delegate.Result, error) {
		calls++
		return delegate.Result{Output: map[string]any{"summary": fmt.Sprint("wrong-", calls), "score": 1}, Tokens: 10}, nil
	}))
	childWorkDir := t.TempDir()
	runner := func(ctx context.Context, req SubbotRequest) (map[string]any, error) {
		children++
		id := fmt.Sprint("child-", children)
		if err := st.SetSubbotChild(ctx, req.ParentRunID, req.ReattachKey, id); err != nil {
			return nil, err
		}
		exec := model.NewClawExecutor(model.NewRegistry(), child, model.WithBackendRegistry(reg))
		eng := New(child, st, exec, WithWorkDir(childWorkDir), WithParentRunID(req.ParentRunID), WithParentNodeID(req.NodeID), WithOutputCorrectionBudget(1))
		return nil, eng.Run(ctx, id, nil)
	}
	eng := New(parent, st, newStubExecutor(), WithSubbotRunner(runner), WithWorkDir(t.TempDir()))
	ctx := context.Background()
	if err := eng.Run(ctx, "parent", nil); err == nil {
		t.Fatal("invalid child succeeded")
	}
	if calls != 2 {
		t.Fatalf("first child calls=%d", calls)
	}
	if err := eng.Resume(ctx, "parent", nil); err == nil {
		t.Fatal("invalid resumed child succeeded")
	}
	if calls != 3 {
		t.Fatalf("parent resume resets child allowance: calls=%d want 3", calls)
	}
	run, err := st.LoadRun(ctx, "child-2")
	if err != nil {
		t.Fatal(err)
	}
	if ep := run.OutputCorrections["my_agent"]; ep.Attempts != 1 {
		t.Fatalf("seeded ledger=%+v", ep)
	}
}
