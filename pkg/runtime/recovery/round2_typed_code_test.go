package recovery_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/store"
)

type refusingExec struct {
	mu       sync.Mutex
	calls    int
	askFirst bool
}

func (x *refusingExec) Execute(_ context.Context, node ir.Node, _ map[string]any) (map[string]any, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.calls++
	if x.askFirst && x.calls == 1 {
		return nil, &model.ErrNeedsInteraction{NodeID: node.NodeID(), Questions: map[string]any{"q": "which?"}, SessionID: "s1", Backend: delegate.BackendClaudeCode}
	}
	return map[string]any{"_tokens": 1}, &delegate.ErrModelUnavailable{Provider: "claude_code", Model: "claude-opus-4-7", Detail: "key not allowed to access model"}
}

func oneAgentWF(interaction ir.InteractionMode) *ir.Workflow {
	a := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "worker"}, LLMFields: ir.LLMFields{Backend: delegate.BackendClaudeCode}}
	a.InteractionFields = ir.InteractionFields{Interaction: interaction}
	return &ir.Workflow{
		Name: "typed", Entry: "worker",
		Nodes:   map[string]ir.Node{"worker": a, "done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}},
		Edges:   []*ir.Edge{{From: "worker", To: "done"}},
		Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{}, Vars: map[string]*ir.Var{},
	}
}

func TestR2TypedModelRefusalAcrossSurfaces(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	for _, c := range []struct {
		name     string
		dispatch bool
	}{{"with DefaultRecipes dispatch (cloud runner, iterion run, dispatcher)", true}, {"no dispatch (iterion resume, CLI/studio subbot child engines)", false}} {
		st, _ := store.New(t.TempDir())
		opts := []runtime.EngineOption{runtime.WithWorkDir(t.TempDir())}
		if c.dispatch {
			opts = append(opts, runtime.WithRecoveryDispatch(recovery.Dispatch(recovery.DefaultRecipes())))
		}
		x := &refusingExec{}
		err := runtime.New(oneAgentWF(ir.InteractionNone), st, x, opts...).Run(context.Background(), "r", nil)
		r, _ := st.LoadRun(context.Background(), "r")
		if r.FailureCode != "MODEL_UNAVAILABLE" {
			t.Errorf("typed refusal lost on %s: %s", c.name, r.FailureCode)
		}
		t.Logf("trunk, %s: calls=%d status=%s storedCode=%s err=%.120v", c.name, x.calls, r.Status, r.FailureCode, err)
	}
	// Re-invocation after an answered ask_user (no recovery dispatch on that path at all).
	st, _ := store.New(t.TempDir())
	x := &refusingExec{askFirst: true}
	opts := []runtime.EngineOption{runtime.WithWorkDir(t.TempDir()), runtime.WithRecoveryDispatch(recovery.Dispatch(recovery.DefaultRecipes()))}
	err := runtime.New(oneAgentWF(ir.InteractionHuman), st, x, opts...).Run(context.Background(), "r2", nil)
	t.Logf("ask_user pause: err=%v paused=%v", err, errors.Is(err, runtime.ErrRunPaused))
	err = runtime.New(oneAgentWF(ir.InteractionHuman), st, x, opts...).Resume(context.Background(), "r2", map[string]any{"q": "this one"})
	r, _ := st.LoadRun(context.Background(), "r2")
	if r.FailureCode != "MODEL_UNAVAILABLE" {
		t.Errorf("typed refusal lost after ask_user: %s", r.FailureCode)
	}
	t.Logf("re-invocation after the answer: calls=%d status=%s storedCode=%s err=%.160v", x.calls, r.Status, r.FailureCode, err)
}
