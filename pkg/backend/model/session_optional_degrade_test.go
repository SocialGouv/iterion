package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// sessionPickyBackend fails any task that carries a SessionID and
// succeeds on a fresh one — the shape of a CLI backend whose session
// files died with the sandbox container (a cloud resume replaces the
// container, `claude --resume <id>` then errors on every attempt).
type sessionPickyBackend struct {
	name  string
	fail  error
	tasks []delegate.Task
}

func (b *sessionPickyBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.tasks = append(b.tasks, task)
	res := delegate.Result{
		BackendName: b.name,
		Duration:    time.Millisecond,
		Output:      map[string]any{"ok": true},
	}
	if task.SessionID != "" {
		return delegate.Result{BackendName: b.name, Duration: time.Millisecond}, b.fail
	}
	return res, nil
}

func sessionDegradeExecutor(fail error) (*ClawExecutor, *sessionPickyBackend, []chainElement) {
	be := &sessionPickyBackend{name: delegate.BackendClaudeCode, fail: fail}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, be)
	e := newFallbackExecutor(reg, EventHooks{})
	return e, be, []chainElement{{Label: "primary"}}
}

func sessionDegradeBuilder(e *ClawExecutor, optional bool) func(context.Context, string) (*delegate.Task, error) {
	return func(_ context.Context, _ string) (*delegate.Task, error) {
		return &delegate.Task{
			NodeID:          "plan_revise",
			Model:           "claude-opus-5",
			SessionID:       "dead-session",
			SessionOptional: optional,
		}, nil
	}
}

// TestOptionalSessionDegradesToFresh pins the inherit_if_available
// contract past the resume boundary: when the upstream session id
// resolves but its backing state no longer loads, the executor retries
// ONCE with the session dropped instead of failing the node forever
// (lived on branch-improve-loop's plan_revise: error_during_execution
// in ~2.6s on every resume, run wedged).
func TestOptionalSessionDegradesToFresh(t *testing.T) {
	e, be, chain := sessionDegradeExecutor(errors.New("delegate: claude-code error: subtype=error_during_execution"))
	build := e.newElementBuilder("plan_revise", delegate.BackendClaudeCode, nil, sessionDegradeBuilder(e, true))

	out, err := e.dispatchChain(context.Background(), "plan_revise", chain, "claude-opus-5", build)
	if err != nil {
		t.Fatalf("optional session should degrade to fresh, got: %v", err)
	}
	if len(be.tasks) != 2 {
		t.Fatalf("want 2 attempts (resume, then fresh), got %d", len(be.tasks))
	}
	if be.tasks[0].SessionID != "dead-session" {
		t.Errorf("first attempt should carry the session id, got %q", be.tasks[0].SessionID)
	}
	if be.tasks[1].SessionID != "" || be.tasks[1].ForkSession || be.tasks[1].SessionFingerprint != "" {
		t.Errorf("fresh retry must drop every session field, got %+v", be.tasks[1])
	}
	if got := out.Result.Output["ok"]; got != true {
		t.Errorf("fresh attempt's output lost: %v", out.Result.Output)
	}
}

// TestRequiredSessionDoesNotDegrade: plain `inherit`/`fork` asked for
// continuity unconditionally — a failure keeps failing loudly.
func TestRequiredSessionDoesNotDegrade(t *testing.T) {
	e, be, chain := sessionDegradeExecutor(errors.New("delegate: claude-code error: subtype=error_during_execution"))
	build := e.newElementBuilder("plan_revise", delegate.BackendClaudeCode, nil, sessionDegradeBuilder(e, false))

	if _, err := e.dispatchChain(context.Background(), "plan_revise", chain, "claude-opus-5", build); err == nil {
		t.Fatal("required session must not degrade to fresh")
	}
	for _, task := range be.tasks {
		if task.SessionID == "" {
			t.Fatal("required session was retried fresh")
		}
	}
}

// TestOptionalSessionAuthFailureDoesNotDegrade: an auth/usage-window
// failure is credential-level — a fresh session hits the same wall, so
// the typed error must surface promptly instead of paying a blind retry.
func TestOptionalSessionAuthFailureDoesNotDegrade(t *testing.T) {
	e, be, chain := sessionDegradeExecutor(&delegate.ErrAuthFailed{Provider: delegate.BackendClaudeCode, Detail: "401"})
	build := e.newElementBuilder("plan_revise", delegate.BackendClaudeCode, nil, sessionDegradeBuilder(e, true))

	_, err := e.dispatchChain(context.Background(), "plan_revise", chain, "claude-opus-5", build)
	if err == nil {
		t.Fatal("auth failure must surface, not degrade")
	}
	var authErr *delegate.ErrAuthFailed
	if !errors.As(err, &authErr) {
		t.Fatalf("typed auth error lost: %v", err)
	}
	for _, task := range be.tasks {
		if task.SessionID == "" {
			t.Fatal("auth failure must not trigger the fresh-session retry")
		}
	}
}
