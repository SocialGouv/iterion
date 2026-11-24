package runtime

import (
	"context"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

func TestCorrectionBudgetUsesFullLoopVector(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	wf := validationWorkflow()
	node := wf.Nodes["my_agent"].(*ir.AgentNode)
	node.Backend = delegate.BackendClaudeCode
	wf.Loops = map[string]*ir.Loop{"inner": {Name: "inner", Body: map[string]bool{"my_agent": true}}, "outer": {Name: "outer", Body: map[string]bool{"entry": true}}}
	wf.Schemas["MySchema"].Fields[0].EnumValues = []string{"pass"}
	calls := 0
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, adverseBackendFunc(func(context.Context, delegate.Task) (delegate.Result, error) {
		calls++
		summary := "pass"
		if calls%2 == 1 {
			summary = fmt.Sprint("wrong-", calls)
		}
		return delegate.Result{Output: map[string]any{"summary": summary, "score": 1}, Tokens: 1}, nil
	}))
	st := tmpStore(t)
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, "nested", wf.Name, nil); err != nil {
		t.Fatal(err)
	}
	exec := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(reg))
	eng := New(wf, st, exec, WithWorkDir(t.TempDir()), WithLogger(iterlog.Nop()), WithOutputCorrectionBudget(1))
	rs := &runState{ctx: ctx, runID: "nested", loopCounters: map[string]int{"inner": 0, "outer": 0}}
	for outer := 0; outer < 2; outer++ {
		rs.loopCounters["outer"] = outer
		if _, err := eng.executeWithResources(ctx, rs, node, nil); err != nil {
			t.Fatalf("outer=%d calls=%d inherited another invocation's budget: %v", outer, calls, err)
		}
	}
	if calls != 4 {
		t.Fatalf("calls=%d want 4", calls)
	}
	run, err := st.LoadRun(ctx, "nested")
	if err != nil {
		t.Fatal(err)
	}
	if len(run.OutputCorrections) != 2 {
		t.Fatalf("ledgers=%+v", run.OutputCorrections)
	}
}

func TestCorrectionLegacyKeyIsClaimedOnlyOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	wf := validationWorkflow()
	wf.Loops = map[string]*ir.Loop{"inner": {Name: "inner", Body: map[string]bool{"my_agent": true}}, "outer": {Name: "outer", Body: map[string]bool{"entry": true}}}
	st := tmpStore(t)
	ctx := context.Background()
	if _, err := st.CreateRun(ctx, "migration", wf.Name, nil); err != nil {
		t.Fatal(err)
	}
	eng := New(wf, st, newStubExecutor(), WithWorkDir(t.TempDir()))
	rs := &runState{ctx: ctx, runID: "migration", loopCounters: map[string]int{"inner": 0, "outer": 0}}
	legacy := "inv_" + correctionFingerprint("node=my_agent;branch=;loops=inner=0")
	ep := store.OutputCorrectionEpisode{EpisodeID: legacy, NodeID: "my_agent", Budget: 2, Attempts: 2, Status: correctionStatusExhausted}
	if err := eng.persistCorrectionEpisode(ctx, rs.runID, legacy, ep); err != nil {
		t.Fatal(err)
	}
	key, id := eng.correctionInvocationIdentity(rs, "my_agent")
	migrated, found, err := eng.loadInvocationCorrectionEpisode(ctx, rs, "my_agent", key, id)
	if err != nil || !found || migrated.Attempts != 2 || migrated.EpisodeID != key {
		t.Fatalf("migration: %+v found=%v err=%v", migrated, found, err)
	}
	rs.loopCounters["outer"] = 1
	key, id = eng.correctionInvocationIdentity(rs, "my_agent")
	if _, found, err := eng.loadInvocationCorrectionEpisode(ctx, rs, "my_agent", key, id); err != nil || found {
		t.Fatalf("legacy key reused: found=%v err=%v", found, err)
	}
	eng.workflow.Loops = nil
	if key, _ := eng.correctionInvocationIdentity(rs, "done"); key != "done" {
		t.Fatalf("non-loop key=%s", key)
	}
}
