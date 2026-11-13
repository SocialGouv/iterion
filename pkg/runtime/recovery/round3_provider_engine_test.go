package recovery_test

import (
	"context"
	"sync"
	"testing"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
	"github.com/SocialGouv/iterion/pkg/store"
)

// r3OnceThenOK fails its first call with err, then answers.
type r3OnceThenOK struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (x *r3OnceThenOK) Execute(_ context.Context, _ ir.Node, _ map[string]any) (map[string]any, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.calls++
	if x.calls == 1 {
		return map[string]any{"_tokens": 10}, x.err
	}
	return map[string]any{"text": "answer", "_tokens": 10}, nil
}

func r3AgentWF() *ir.Workflow {
	a := &ir.AgentNode{BaseNode: ir.BaseNode{ID: "worker"}, LLMFields: ir.LLMFields{Backend: delegate.BackendClaw, Model: "openai_compatible/scaleway/deepseek-v4.1-flash"}}
	return &ir.Workflow{
		Name: "r3", Entry: "worker",
		Nodes:   map[string]ir.Node{"worker": a, "done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}},
		Edges:   []*ir.Edge{{From: "worker", To: "done"}},
		Schemas: map[string]*ir.Schema{}, Prompts: map[string]*ir.Prompt{}, Vars: map[string]*ir.Var{},
	}
}

// A run whose node meets ONE sample-dependent provider 400 (the second
// attempt is a fresh conversation and passes): does the run finish?
func TestR3G9SampleDependent400Heals(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	for _, c := range []struct{ name, msg string }{
		{"z.ai 1301 (input-or-GENERATED content flagged)", "系统检测到输入或生成内容可能包含不安全或敏感内容，请您避免输入易产生敏感内容的提示语，感谢您的配合。"},
		{"anthropic prompt is too long (context overflow)", "prompt is too long: 208310 tokens > 200000 maximum"},
		{"tool screenshot over the image cap", "messages.4.content.1.image.source.base64: image exceeds 5 MB maximum: 6291456 bytes > 5242880 bytes"},
	} {
		st, _ := store.New(t.TempDir())
		x := &r3OnceThenOK{err: &api.APIError{Provider: "openai", StatusCode: 400, Message: c.msg}}
		eng := runtime.New(r3AgentWF(), st, x, runtime.WithWorkDir(t.TempDir()), runtime.WithRecoveryDispatch(recovery.Dispatch(recovery.DefaultRecipes())))
		err := eng.Run(context.Background(), "r3", nil)
		r, _ := st.LoadRun(context.Background(), "r3")
		if err != nil || x.calls != 2 || r.Status != "finished" {
			t.Errorf("%s: calls=%d status=%s code=%s err=%v", c.name, x.calls, r.Status, r.FailureCode, err)
		}
	}
}
