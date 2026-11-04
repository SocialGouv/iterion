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
)

// r3ReviewBackend answers like a claude_code reviewer that could not emit
// structured output (prose parse fallback), then its re-ask dies on a
// network reset.
type r3ReviewBackend struct {
	mu    sync.Mutex
	calls int
	prose string
}

func (b *r3ReviewBackend) Execute(_ context.Context, _ delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls++
	if b.calls == 1 {
		return delegate.Result{Output: map[string]any{"text": b.prose}, ParseFallback: true, Tokens: 10, BackendName: delegate.BackendClaudeCode}, nil
	}
	return delegate.Result{}, errors.New("claude_code: stream: read tcp 10.0.0.7:51234->160.79.104.10:443: read: connection reset by peer")
}

func r3JudgeWF() (*ir.Workflow, *ir.JudgeNode) {
	j := &ir.JudgeNode{
		BaseNode:     ir.BaseNode{ID: "review"},
		LLMFields:    ir.LLMFields{Backend: delegate.BackendClaudeCode, Model: "claude-opus-5-5", UserPrompt: "ask"},
		SchemaFields: ir.SchemaFields{OutputSchema: "verdict"},
	}
	wf := &ir.Workflow{
		Name: "r3prose", Entry: "review",
		Nodes:   map[string]ir.Node{"review": j},
		Prompts: map[string]*ir.Prompt{"ask": {Body: "Review the diff."}},
		Schemas: map[string]*ir.Schema{"verdict": {Name: "verdict", Fields: []*ir.SchemaField{{Name: "verdict", Type: ir.FieldTypeBool}, {Name: "reason", Type: ir.FieldTypeString}}}},
		Vars:    map[string]*ir.Var{},
	}
	return wf, j
}

// Review prose leaves the network fault's classification unchanged.
func TestR3_G9ModelProseDecidesTheCode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	for _, prose := range []string{
		"Looks good overall; the retry helper is correct.",
		"Looks good overall; the client now throws when the server answers with status code 400, which the caller handles.",
	} {
		wf, node := r3JudgeWF()
		backends := delegate.NewRegistry()
		fake := &r3ReviewBackend{prose: prose}
		backends.Register(delegate.BackendClaudeCode, fake)
		exec := model.NewClawExecutor(model.NewRegistry(), wf, model.WithBackendRegistry(backends), model.WithRetryPolicy(model.RetryPolicy{MaxAttempts: 1}))
		_, err := exec.Execute(model.WithRunID(context.Background(), "r3p"), node, map[string]any{})
		if code := recovery.Classify(err); code != runtime.ErrCodeNetworkTransient {
			t.Errorf("prose=%q code=%s err=%v", prose, code, err)
		}
	}
}
