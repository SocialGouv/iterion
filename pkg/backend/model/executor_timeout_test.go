package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// ctxBlockingBackend blocks until its execution context is cancelled, then
// returns that context's error. It lets a test prove the per-node `timeout:`
// derives a bounded context that fires (the backend would otherwise never
// return on a background context).
type ctxBlockingBackend struct{}

func (ctxBlockingBackend) Execute(ctx context.Context, _ delegate.Task) (delegate.Result, error) {
	<-ctx.Done()
	return delegate.Result{}, ctx.Err()
}

// immediateBackend returns success without consulting the context, so a
// generous-but-set timeout can be shown NOT to break the happy path.
type immediateBackend struct{}

func (immediateBackend) Execute(_ context.Context, task delegate.Task) (delegate.Result, error) {
	return delegate.Result{
		Output:      map[string]interface{}{"ok": true, "model": task.Model},
		BackendName: delegate.BackendClaudeCode,
	}, nil
}

func timeoutTestExecutor(reg *delegate.Registry) *ClawExecutor {
	return &ClawExecutor{
		retry:           RetryPolicy{MaxAttempts: 1, BackoffBase: time.Millisecond},
		logger:          iterlog.Nop(),
		backendRegistry: reg,
	}
}

func timeoutAgentNode(id, timeout string) *ir.AgentNode {
	n := &ir.AgentNode{}
	n.ID = id
	n.Backend = delegate.BackendClaudeCode
	n.Timeout = timeout
	return n
}

// safetyNet bounds the PARENT context so a regression in the per-node bound
// fails this test instead of hanging it.
//
// The node bound is the only deadline on the path (retryDelegateLoop and
// dispatchWithProviderFallback add none), so against a backend that blocks on
// its context, dropping the bound means Execute never returns: `go test`
// panics on the package timeout minutes later and takes every other result in
// the package with it. 5s is far enough above the 20ms node bound to keep the
// elapsed assertion a clear discriminator.
const safetyNet = 5 * time.Second

// TestNodeTimeout_Enforced: a short per-node timeout bounds a backend that
// would otherwise block forever, and the node fails with a deadline error.
func TestNodeTimeout_Enforced(t *testing.T) {
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, ctxBlockingBackend{})

	e := timeoutTestExecutor(reg)
	node := timeoutAgentNode("worker", "20ms")

	ctx, cancel := context.WithTimeout(context.Background(), safetyNet)
	defer cancel()

	start := time.Now()
	_, err := e.Execute(ctx, node, map[string]interface{}{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected a deadline error from the per-node timeout, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}
	// Both the node bound and the safety net produce DeadlineExceeded, so the
	// error alone cannot tell them apart — the elapsed time is what proves the
	// 20ms NODE bound fired and not the 5s parent.
	if elapsed > time.Second {
		t.Fatalf("the parent safety net fired, not the per-node timeout: elapsed %v (want ~20ms)", elapsed)
	}
}

// TestNodeTimeout_HappyPathUnaffected: a set-but-not-exceeded timeout leaves
// a fast node untouched (the bound only fails on expiry).
func TestNodeTimeout_HappyPathUnaffected(t *testing.T) {
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, immediateBackend{})

	e := timeoutTestExecutor(reg)
	node := timeoutAgentNode("worker", "1h")

	out, err := e.Execute(context.Background(), node, map[string]interface{}{})
	if err != nil {
		t.Fatalf("expected success with a generous timeout, got %v", err)
	}
	if out["ok"] != true {
		t.Fatalf("expected ok=true output, got %v", out)
	}
}

// TestNodeTimeout_EnvDefaultExpanded: a `${VAR:-default}` timeout with the var
// unset expands to its default and is enforced like a literal.
func TestNodeTimeout_EnvDefaultExpanded(t *testing.T) {
	t.Setenv("ITERION_TEST_NODE_TIMEOUT_UNSET", "")
	reg := delegate.NewRegistry()
	reg.Register(delegate.BackendClaudeCode, ctxBlockingBackend{})

	e := timeoutTestExecutor(reg)
	node := timeoutAgentNode("worker", "${ITERION_TEST_NODE_TIMEOUT_UNSET:-20ms}")

	ctx, cancel := context.WithTimeout(context.Background(), safetyNet)
	defer cancel()

	start := time.Now()
	_, err := e.Execute(ctx, node, map[string]interface{}{})
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected env-default timeout to fire with DeadlineExceeded, got %v", err)
	}
	// Guards the expansion specifically: if ${VAR:-20ms} ever stops resolving,
	// the parse fails, the bound is skipped by design, and only the elapsed
	// time distinguishes that from a working expansion.
	if elapsed > time.Second {
		t.Fatalf("the env default did not expand to an enforced bound: elapsed %v (want ~20ms)", elapsed)
	}
}

// A timeout that does not resolve to a positive duration is a deliberate
// silent skip, not an error: the compile-time diagnostic already rejects a
// malformed one, so the runtime guard is defensive. Pinned so a later refactor
// cannot quietly turn the skip into a failed node.
func TestNodeTimeout_NonPositiveOrUnparseableIsSkipped(t *testing.T) {
	for _, tc := range []struct{ name, timeout string }{
		{"zero", "0s"},
		{"negative", "-5s"},
		{"unparseable", "not-a-duration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := delegate.NewRegistry()
			reg.Register(delegate.BackendClaudeCode, immediateBackend{})

			e := timeoutTestExecutor(reg)
			node := timeoutAgentNode("worker", tc.timeout)

			ctx, cancel := context.WithTimeout(context.Background(), safetyNet)
			defer cancel()

			out, err := e.Execute(ctx, node, map[string]interface{}{})
			if err != nil {
				t.Fatalf("timeout %q should be skipped, not applied: %v", tc.timeout, err)
			}
			if out["ok"] != true {
				t.Fatalf("expected ok=true output, got %v", out)
			}
		})
	}
}
