package model

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// r6ForkBackend: the fork of "parent" is gone (unclassified refusal); the
// fresh retry spends a real session and then fails too.
type r6ForkBackend struct {
	mu    sync.Mutex
	tasks []delegate.Task
}

func (b *r6ForkBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tasks = append(b.tasks, task)
	if task.SessionID == "parent" {
		return delegate.Result{BackendName: delegate.BackendClaudeCode, Output: map[string]any{}}, errors.New("No conversation found with session ID: parent")
	}
	return delegate.Result{BackendName: delegate.BackendClaudeCode, SessionID: "fresh-1", CostUSD: 3, CostIsSessionTotal: true, Tokens: 1000,
		Output: map[string]any{"_cost_usd": 3.0, "_tokens": 1000}}, errors.New("error_during_execution")
}

// A fork whose parent session is gone degrades to a fresh session; when that
// fresh call fails too, its failure output describes the fresh session, never
// a fork of the parent — the budget would otherwise subtract the parent's
// total from the fresh session's spend.
func TestR6_ForkDegradeFailureIsNotStampedAsAFork(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	b := &r6ForkBackend{}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, b)
	exec := NewClawExecutor(NewRegistry(), verdictWorkflow(), WithBackendRegistry(reg),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}))
	node := verdictJudge(delegate.BackendClaudeCode)
	node.Session = ir.SessionFork
	input := map[string]any{delegate.SessionIDKey: "parent", delegate.SessionOptionalKey: true}
	out, err := exec.executeBackend(context.Background(), node, input)
	for i, task := range b.tasks {
		t.Logf("call %d: session=%q fork=%v optional=%v", i+1, task.SessionID, task.ForkSession, task.SessionOptional)
	}
	t.Logf("err=%v", err)
	t.Logf("failure output=%v", out)
	if len(b.tasks) != 2 || b.tasks[1].SessionID != "" || out["_session_id"] != "fresh-1" {
		t.Fatalf("scenario broken: want the fork refused then one fresh call, got %d call(s), output %v", len(b.tasks), out)
	}
	if out["_session_forked_from"] != nil {
		t.Errorf("the fresh session's failure is stamped as a fork of %v", out["_session_forked_from"])
	}
}
