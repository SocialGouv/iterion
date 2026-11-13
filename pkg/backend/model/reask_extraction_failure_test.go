package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/claw/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

func TestRVA_ReaskErrorStillGetsTheExtraction(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("ANTHROPIC_BASE_URL", "")
	t.Setenv("ANTHROPIC_API_KEY", "test-key")
	extraction := newMockClient(toolUseEvents("x1", "structured_output", `{"verdict":true,"reason":"the diff is fine"}`, 200, 50))
	reg := NewRegistry()
	reg.Register("anthropic", func(string) (api.APIClient, error) { return extraction, nil })
	backend := &stubBackend{
		results: []delegate.Result{{Output: map[string]any{"text": "The verdict is pass: the diff is fine."}, ParseFallback: true, SessionID: "sess-1", Tokens: 10, BackendName: delegate.BackendClaudeCode}},
		errors:  []error{nil, errors.New("claude_code: No conversation found with session ID: sess-1")},
	}
	dreg := delegate.NewRegistry()
	dreg.Register(delegate.BackendClaudeCode, backend)
	exec := NewClawExecutor(reg, verdictWorkflow(), WithBackendRegistry(dreg), WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}))
	out, err := exec.executeBackend(context.Background(), verdictJudge(delegate.BackendClaudeCode), map[string]any{})
	t.Logf("delegations=%d extraction calls=%d err=%v verdict=%v reason=%v", backend.calls, len(extraction.getCalls()), err, out["verdict"], out["reason"])
	if err != nil || len(extraction.getCalls()) != 1 || out["verdict"] != true {
		t.Errorf("the re-ask's failure is no longer rescued by the last-resort extraction: %v", err)
	}
}
