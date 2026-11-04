package model

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// r3ProseBackend answers like the claude_code CLI when it cannot emit
// structured output: a prose parse fallback (no ParseError) for the primary
// model, a valid structured answer for the rescue model.
type r3ProseBackend struct {
	mu     sync.Mutex
	models []string
}

func (b *r3ProseBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	b.models = append(b.models, task.Model)
	b.mu.Unlock()
	if strings.Contains(task.Model, "rescue") {
		return delegate.Result{Output: map[string]any{"verdict": true, "reason": "rescued"}, Tokens: 10, BackendName: delegate.BackendClaudeCode}, nil
	}
	return delegate.Result{Output: map[string]any{"text": "I approve this change."}, ParseFallback: true, Tokens: 10, BackendName: delegate.BackendClaudeCode}, nil
}

// Same node, same declared rescue route, same fault class (the primary model
// never produces a structured answer): which route serves, per backend?
func TestR3_ParseFailureFallbackParity(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	// claw: malformed structured_output on every primary turn, rescue valid.
	var scripts [][]api.StreamEvent
	for i := 0; i < 8; i++ {
		scripts = append(scripts, toolUseEvents("p", "structured_output", `{"verdict":tru`, 50, 20))
	}
	primary := newMockClient(scripts...)
	rescue := newMockClient(toolUseEvents("ok", "structured_output", `{"verdict":true,"reason":"rescued"}`, 30, 10))
	reg := NewRegistry()
	reg.Register("test", func(string) (api.APIClient, error) { return primary, nil })
	reg.Register("alt", func(string) (api.APIClient, error) { return rescue, nil })
	exec := newTestClawExecutor(reg, verdictWorkflow(), WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}))
	node := verdictJudge("claw")
	node.Fallbacks = []ir.Fallback{{Name: "rescue", Backend: "claw", Model: "alt/alt-model"}}
	out, err := exec.Execute(WithRunID(context.Background(), "run-claw"), node, map[string]any{})
	if err != nil || out["_served_by"] != "rescue" || len(rescue.getCalls()) != 1 {
		t.Fatalf("claw: out=%v err=%v primary=%d rescue=%d", out, err, len(primary.getCalls()), len(rescue.getCalls()))
	}

	// claude_code: prose parse fallback on the primary, rescue valid.
	fake := &r3ProseBackend{}
	exec2 := newTestClawExecutor(NewRegistry(), verdictWorkflow(), WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}))
	exec2.backendRegistry.Register(delegate.BackendClaudeCode, fake)
	node2 := verdictJudge(delegate.BackendClaudeCode)
	node2.Model = "claude-primary"
	node2.Fallbacks = []ir.Fallback{{Name: "rescue", Backend: delegate.BackendClaudeCode, Model: "claude-rescue"}}
	out2, err2 := exec2.Execute(WithRunID(context.Background(), "run-cc"), node2, map[string]any{})
	if err2 != nil || out2["_served_by"] != "rescue" {
		t.Fatalf("claude_code: out=%v err=%v calls=%v", out2, err2, fake.models)
	}
}
