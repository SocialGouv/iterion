package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

// loopExecutor builds a minimal ClawExecutor for exercising retryDelegateLoop
// directly. BackoffBase is sub-millisecond so the exponential waits don't slow
// the test; nil hooks are fine (the loop guards every hook call).
func loopExecutor() *ClawExecutor {
	return &ClawExecutor{
		retry:  RetryPolicy{BackoffBase: time.Microsecond}, // 3 standard / 6 transient
		logger: iterlog.Nop(),
	}
}

func TestRetryDelegateLoop_NetworkUsesTransientBudget(t *testing.T) {
	e := loopExecutor()
	calls := 0
	_, err := e.retryDelegateLoop(context.Background(), "n", delegate.BackendClaudeCode, func() (delegate.Result, error) {
		calls++
		return delegate.Result{}, errors.New("fetch failed")
	})
	if err == nil {
		t.Fatal("expected failure once the budget is exhausted")
	}
	if calls != DefaultMaxAttemptsTransient {
		t.Fatalf("network error should ride the transient budget: got %d calls, want %d", calls, DefaultMaxAttemptsTransient)
	}
}

func TestRetryDelegateLoop_SignalUsesStandardBudget(t *testing.T) {
	e := loopExecutor()
	calls := 0
	_, err := e.retryDelegateLoop(context.Background(), "n", delegate.BackendClaudeCode, func() (delegate.Result, error) {
		calls++
		return delegate.Result{}, errors.New("signal: killed") // retryable, not network
	})
	if err == nil {
		t.Fatal("expected failure")
	}
	if calls != DefaultMaxAttempts {
		t.Fatalf("signal kill should use the standard budget: got %d calls, want %d", calls, DefaultMaxAttempts)
	}
}

func TestRetryDelegateLoop_PermanentNoRetry(t *testing.T) {
	e := loopExecutor()
	calls := 0
	_, _ = e.retryDelegateLoop(context.Background(), "n", delegate.BackendClaudeCode, func() (delegate.Result, error) {
		calls++
		return delegate.Result{}, errors.New("exit status 1") // application error
	})
	if calls != 1 {
		t.Fatalf("permanent error must not be retried: got %d calls, want 1", calls)
	}
}

func TestRetryDelegateLoop_RecoversMidBudget(t *testing.T) {
	e := loopExecutor()
	calls := 0
	res, err := e.retryDelegateLoop(context.Background(), "n", delegate.BackendClaudeCode, func() (delegate.Result, error) {
		calls++
		if calls < 3 {
			return delegate.Result{}, errors.New("ECONNRESET") // network blip
		}
		return delegate.Result{Output: map[string]any{"ok": true}}, nil
	})
	if err != nil {
		t.Fatalf("expected recovery on the 3rd attempt, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected exactly 3 attempts (2 fail + 1 success), got %d", calls)
	}
	if res.Output["ok"] != true {
		t.Fatalf("expected the successful result to propagate, got %v", res.Output)
	}
}

func TestEffectiveMaxAttempts(t *testing.T) {
	rp := RetryPolicy{} // defaults: 3 standard, 6 transient
	if got := rp.effectiveMaxAttempts(errors.New("fetch failed")); got != DefaultMaxAttemptsTransient {
		t.Fatalf("network err: effectiveMaxAttempts = %d, want %d", got, DefaultMaxAttemptsTransient)
	}
	if got := rp.effectiveMaxAttempts(errors.New("exit status 137")); got != DefaultMaxAttempts {
		t.Fatalf("non-network (signal) err: effectiveMaxAttempts = %d, want %d", got, DefaultMaxAttempts)
	}
	if got := rp.effectiveMaxAttempts(nil); got != DefaultMaxAttempts {
		t.Fatalf("nil err: effectiveMaxAttempts = %d, want %d", got, DefaultMaxAttempts)
	}
}

func TestMaxAttemptsTransientNeverSmallerThanStandard(t *testing.T) {
	rp := RetryPolicy{MaxAttempts: 10, MaxAttemptsTransient: 2}
	if got := rp.maxAttemptsTransient(); got != 10 {
		t.Fatalf("transient budget must clamp up to standard: got %d, want 10", got)
	}
}

func TestIsDelegateRetryableNetwork(t *testing.T) {
	// A connectivity failure stringified through the claude_code wrapper.
	netErr := errors.New("delegate: claude-code failed: claude session ended without result message; fetch failed")
	if !isDelegateRetryable(netErr) {
		t.Fatal("expected network-tagged delegate error to be retryable")
	}
	// A genuine permanent failure (no network marker, non-signal exit) stays
	// non-retryable — we must not loop forever on a real error.
	permanent := errors.New("delegate: claude-code failed: claude session ended without result message (cli_exit_code=2)")
	if isDelegateRetryable(permanent) {
		t.Fatal("expected bare exit-2 to be non-retryable")
	}
}

func TestIsRetryableRawNetError(t *testing.T) {
	if !isRetryable(errors.New("dial tcp: lookup api.anthropic.com: no such host")) {
		t.Fatal("expected raw net error to be retryable for the claw backend")
	}
	if isRetryable(errors.New("missing required field stats")) {
		t.Fatal("schema validation error must not be retryable")
	}
}
