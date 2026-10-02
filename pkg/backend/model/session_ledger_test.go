package model

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/backend/secretguard"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
)

// ledgerStep is one call of ledgerBackend: what the call returns, and the
// background work its process leaves running.
type ledgerStep struct {
	res        delegate.Result
	err        error
	terminated []string
}

// ledgerBackend keeps the session ledger's contract the way claude_code does:
// a call that resumes a session is told that session's entry, and settles it
// under the session its process ran once it ends.
type ledgerBackend struct {
	mu     sync.Mutex
	script []ledgerStep
	tasks  []delegate.Task
	told   [][]string
}

func (b *ledgerBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var told []string
	if task.SessionLedger != nil && task.SessionID != "" {
		told = task.SessionLedger.Terminated(task.SessionID)
	}
	b.tasks = append(b.tasks, task)
	b.told = append(b.told, told)
	step := b.script[min(len(b.tasks), len(b.script))-1]
	res := step.res
	res.TerminatedBackgroundTasks = step.terminated
	if task.SessionLedger != nil && res.SessionID != "" {
		task.SessionLedger.Settle(res.SessionID, told, step.terminated)
	}
	return res, step.err
}

func ledgerExecutor(be delegate.Backend) *ClawExecutor {
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, be)
	e := newFallbackExecutor(reg, EventHooks{})
	e.retry.MaxAttempts = 3
	return e
}

func TestEveryDelegateTaskCarriesTheRunsSessionLedger(t *testing.T) {
	be := &ledgerBackend{script: []ledgerStep{{res: delegate.Result{BackendName: delegate.BackendClaudeCode, Output: map[string]any{"ok": true}}}}}
	ledger := delegate.NewSessionLedger(nil)
	ctx := WithSessionLedger(context.Background(), ledger)
	if _, err := ledgerExecutor(be).Execute(ctx, fallbackAgentNode("n", delegate.BackendClaudeCode, ""), map[string]any{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(be.tasks) != 1 || be.tasks[0].SessionLedger != delegate.SessionLedger(ledger) {
		t.Fatalf("the delegate task did not carry the run's ledger: %+v", be.tasks)
	}
}

// B-F1 at the chokepoint: a node that resumes a DECLARED session retries in
// the transcript its dead attempt extended — and is told what that attempt
// left running, whatever it was built with.
func TestADeclaredSessionRetryIsToldWhatItsDeadAttemptLeft(t *testing.T) {
	for _, mode := range []ir.SessionMode{ir.SessionInherit, ir.SessionInheritIfAvailable, ir.SessionPersist} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			be := &ledgerBackend{script: []ledgerStep{
				{res: delegate.Result{BackendName: delegate.BackendClaudeCode, SessionID: "s-declared", Output: map[string]any{}},
					err: &delegate.ErrTransient{Reason: "stream closed: connection reset by peer"}, terminated: []string{"attempt-1 agent (local_agent, t9)"}},
				{res: delegate.Result{BackendName: delegate.BackendClaudeCode, SessionID: "s-declared", Output: map[string]any{"ok": true}}},
			}}
			ledger := delegate.NewSessionLedger(map[string][]string{"s-declared": {"upstream agent (local_agent, t1)"}})
			node := fallbackAgentNode("worker", delegate.BackendClaudeCode, "")
			node.Session = mode
			ctx := WithSessionLedger(context.Background(), ledger)
			if _, err := ledgerExecutor(be).Execute(ctx, node, map[string]any{delegate.SessionIDKey: "s-declared"}); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if len(be.told) != 2 {
				t.Fatalf("attempts = %d, want 2", len(be.told))
			}
			if !reflect.DeepEqual(be.told[0], []string{"upstream agent (local_agent, t1)"}) {
				t.Fatalf("attempt 1 was told %v", be.told[0])
			}
			if !reflect.DeepEqual(be.told[1], []string{"attempt-1 agent (local_agent, t9)"}) {
				t.Fatalf("attempt 2 was told %v, want the dead attempt's work (and not the upstream work attempt 1 was already told)", be.told[1])
			}
			if got := ledger.Terminated("s-declared"); got != nil {
				t.Fatalf("ledger = %v after attempt 2 was told and left nothing", got)
			}
		})
	}
}

func TestACarriedSessionRetryIsToldWhatItsDeadAttemptLeft(t *testing.T) {
	be := &ledgerBackend{script: []ledgerStep{
		{res: delegate.Result{BackendName: delegate.BackendClaudeCode, SessionID: "s-fresh", Output: map[string]any{}},
			err: &delegate.ErrTransient{Reason: "stream closed"}, terminated: []string{"agent (local_agent, t9)"}},
		{res: delegate.Result{BackendName: delegate.BackendClaudeCode, SessionID: "s-fresh", Output: map[string]any{"ok": true}}},
	}}
	ledger := delegate.NewSessionLedger(nil)
	ctx := WithSessionLedger(context.Background(), ledger)
	if _, err := ledgerExecutor(be).Execute(ctx, fallbackAgentNode("n", delegate.BackendClaudeCode, ""), map[string]any{}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(be.tasks) != 2 || be.tasks[1].SessionID != "s-fresh" {
		t.Fatalf("the retry did not carry the session: %+v", be.tasks)
	}
	if !reflect.DeepEqual(be.told[1], []string{"agent (local_agent, t9)"}) {
		t.Fatalf("the carried retry was told %v", be.told[1])
	}
}

// The schema re-ask resumes the answer's session in a process of its own.
func TestASchemaReaskIsToldWhatTheAnswersProcessLeft(t *testing.T) {
	be := &ledgerBackend{script: []ledgerStep{
		{res: delegate.Result{BackendName: delegate.BackendClaudeCode, SessionID: "sess-1", Output: map[string]any{"verdict": true}},
			terminated: []string{"checker (local_agent, t4)"}},
		{res: delegate.Result{BackendName: delegate.BackendClaudeCode, SessionID: "sess-1", Output: map[string]any{"verdict": true, "reason": "because"}}},
	}}
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, be)
	exec := NewClawExecutor(NewRegistry(), verdictWorkflow(),
		WithBackendRegistry(reg),
		WithRetryPolicy(RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond}),
	)
	ledger := delegate.NewSessionLedger(nil)
	ctx := WithSessionLedger(context.Background(), ledger)
	if _, err := exec.executeBackend(ctx, verdictJudge(delegate.BackendClaudeCode), map[string]any{}); err != nil {
		t.Fatalf("executeBackend: %v", err)
	}
	if len(be.told) != 2 || !reflect.DeepEqual(be.told[1], []string{"checker (local_agent, t4)"}) {
		t.Fatalf("the re-ask was told %v, want what the answer's process left", be.told)
	}
}

// A claude_code router keeps the CLI's native tools: its task is a claude
// session like any other, and goes through the run's ledger.
func TestTheLLMRouterTaskCarriesTheRunsSessionLedger(t *testing.T) {
	be := &ledgerBackend{script: []ledgerStep{{res: delegate.Result{Output: map[string]any{"selected_route": "agent_a", "reasoning": "r"}}}}}
	exec := newDelegateTestExecutor(be, EventHooks{})
	ledger := delegate.NewSessionLedger(nil)
	node := &ir.RouterNode{BaseNode: ir.BaseNode{ID: "r"}, LLMFields: ir.LLMFields{Backend: "test_backend", SystemPrompt: "sys"}, RouterMode: ir.RouterLLM}
	if _, err := exec.executeLLMRouterUnified(WithSessionLedger(context.Background(), ledger), node, map[string]any{"_route_candidates": []string{"agent_a", "agent_b"}}); err != nil {
		t.Fatalf("router: %v", err)
	}
	if len(be.tasks) != 1 || be.tasks[0].SessionLedger != delegate.SessionLedger(ledger) {
		t.Fatalf("the router task did not carry the run's ledger: %+v", be.tasks)
	}
}

// The CLI names a background task after its command, which carries the
// secrets iterion materialised into it. Every claude task — the router's
// too — carries the mirror that turns them back into placeholders.
func TestEveryClaudeTaskCarriesTheSecretRedactor(t *testing.T) {
	const secret = "sk-live-4f9a8b7c6d5e3a2b1c0d"
	guard := secretguard.New([]secretguard.Secret{{Name: "api_key", Value: secret}}, secretguard.DefaultConfig())
	check := func(t *testing.T, tasks []delegate.Task) {
		t.Helper()
		if len(tasks) != 1 || tasks[0].RedactSecrets == nil || tasks[0].UnmaterializeSecrets == nil {
			t.Fatalf("the task carries no secret redactor: %+v", tasks)
		}
		if got := tasks[0].RedactSecrets("API_KEY=" + secret + " ./server"); strings.Contains(got, secret) {
			t.Fatalf("redacted = %q, still carries the secret", got)
		}
		if span := tasks[0].RedactSecretsSpan; span == 0 || span != guard.LongestLiteral() {
			t.Fatalf("RedactSecretsSpan = %d, want the guard's longest literal (%d)", span, guard.LongestLiteral())
		}
		if got := tasks[0].UnmaterializeSecrets("API_KEY=" + secret + " ./server"); strings.Contains(got, secret) {
			t.Fatalf("unmaterialised = %q, still carries the secret", got)
		}
	}
	t.Run("agent", func(t *testing.T) {
		be := &ledgerBackend{script: []ledgerStep{{res: delegate.Result{BackendName: delegate.BackendClaudeCode, Output: map[string]any{"ok": true}}}}}
		exec := ledgerExecutor(be)
		exec.secretGuard = guard
		if _, err := exec.Execute(context.Background(), fallbackAgentNode("n", delegate.BackendClaudeCode, ""), map[string]any{}); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		check(t, be.tasks)
	})
	t.Run("router", func(t *testing.T) {
		be := &ledgerBackend{script: []ledgerStep{{res: delegate.Result{Output: map[string]any{"selected_route": "agent_a", "reasoning": "r"}}}}}
		exec := newDelegateTestExecutor(be, EventHooks{})
		exec.secretGuard = guard
		node := &ir.RouterNode{BaseNode: ir.BaseNode{ID: "r"}, LLMFields: ir.LLMFields{Backend: "test_backend", SystemPrompt: "sys"}, RouterMode: ir.RouterLLM}
		if _, err := exec.executeLLMRouterUnified(context.Background(), node, map[string]any{"_route_candidates": []string{"agent_a", "agent_b"}}); err != nil {
			t.Fatalf("router: %v", err)
		}
		check(t, be.tasks)
	})
}

// ITERION_SECRETS_REDACT=off turns the sink redaction off, never its Layer 1
// mirror: every claude task — the router's too — still carries an
// UnmaterializeSecrets that turns a known value back into its placeholder
// (docs/secrets.md: "It is not a sink pass: ITERION_SECRETS_REDACT=off leaves
// it on").
func TestEveryClaudeTaskUnmaterialisesWithTheSinkSwitchOff(t *testing.T) {
	const secret = "sk-live-4f9a8b7c6d5e3a2b1c0d"
	t.Setenv("ITERION_SECRETS_REDACT", "off")
	guard := secretguard.New([]secretguard.Secret{{Name: "api_key", Value: secret}}, secretGuardConfigFromEnv())
	check := func(t *testing.T, tasks []delegate.Task) {
		t.Helper()
		if len(tasks) != 1 || tasks[0].UnmaterializeSecrets == nil {
			t.Fatalf("the task carries no unmaterialiser: %+v", tasks)
		}
		if got := tasks[0].UnmaterializeSecrets("API_KEY=" + secret + " ./server"); strings.Contains(got, secret) || !strings.Contains(got, "__ITERION_SECRET_") {
			t.Fatalf("unmaterialised with ITERION_SECRETS_REDACT=off = %q: the mirror followed the sink switch", got)
		}
	}
	t.Run("agent", func(t *testing.T) {
		be := &ledgerBackend{script: []ledgerStep{{res: delegate.Result{BackendName: delegate.BackendClaudeCode, Output: map[string]any{"ok": true}}}}}
		exec := ledgerExecutor(be)
		exec.secretGuard = guard
		if _, err := exec.Execute(context.Background(), fallbackAgentNode("n", delegate.BackendClaudeCode, ""), map[string]any{}); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		check(t, be.tasks)
	})
	t.Run("router", func(t *testing.T) {
		be := &ledgerBackend{script: []ledgerStep{{res: delegate.Result{Output: map[string]any{"selected_route": "agent_a", "reasoning": "r"}}}}}
		exec := newDelegateTestExecutor(be, EventHooks{})
		exec.secretGuard = guard
		node := &ir.RouterNode{BaseNode: ir.BaseNode{ID: "r"}, LLMFields: ir.LLMFields{Backend: "test_backend", SystemPrompt: "sys"}, RouterMode: ir.RouterLLM}
		if _, err := exec.executeLLMRouterUnified(context.Background(), node, map[string]any{"_route_candidates": []string{"agent_a", "agent_b"}}); err != nil {
			t.Fatalf("router: %v", err)
		}
		check(t, be.tasks)
	})
}
