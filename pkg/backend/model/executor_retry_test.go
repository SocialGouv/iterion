package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	clawapi "github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
)

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", errors.New("boom"), false},
		{"local APIError retryable", &APIError{Message: "rate-limit", StatusCode: 429, IsRetryable: true}, true},
		{"local APIError not retryable", &APIError{Message: "bad-req", StatusCode: 400, IsRetryable: false}, false},
		{"clawapi 429", &clawapi.APIError{StatusCode: 429, Message: "rate-limited", Retryable: true}, true},
		{"clawapi 503", &clawapi.APIError{StatusCode: 503, Message: "service unavailable", Retryable: true}, true},
		{"clawapi 400 (Retryable=false)", &clawapi.APIError{StatusCode: 400, Message: "bad", Retryable: false}, false},
		{"clawapi 500 without Retryable flag", &clawapi.APIError{StatusCode: 500, Retryable: false}, false},
		{"wrapped local APIError", fmt.Errorf("model: %w", &APIError{StatusCode: 502, IsRetryable: true}), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isRetryable(c.err); got != c.want {
				t.Errorf("isRetryable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestStatusCodeOf(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, 0},
		{"plain", errors.New("boom"), 0},
		{"local APIError", &APIError{StatusCode: 429}, 429},
		{"clawapi APIError", &clawapi.APIError{StatusCode: 503}, 503},
		{"wrapped clawapi", fmt.Errorf("oops: %w", &clawapi.APIError{StatusCode: 401}), 401},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := statusCodeOf(c.err); got != c.want {
				t.Errorf("statusCodeOf(%v) = %d, want %d", c.err, got, c.want)
			}
		})
	}
}

func TestIsDelegateRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain non-matching", errors.New("application error"), false},
		{"ErrTransient typed", &delegate.ErrTransient{Provider: "claude_code", Reason: "subprocess killed"}, true},
		{"ErrRateLimited typed", &delegate.ErrRateLimited{Provider: "claw", Detail: "quota"}, true},
		{"wrapped ErrTransient", fmt.Errorf("wrap: %w", &delegate.ErrTransient{Reason: "x"}), true},
		{"signal kill", errors.New("subprocess died: signal: killed"), true},
		{"exit status 137 (OOM)", errors.New("exec failed: exit status 137"), true},
		{"exit status 143 (SIGTERM)", errors.New("exec failed: exit status 143"), true},
		{"exit status 128 boundary", errors.New("exit status 128"), true},
		{"exit status 1 (app err)", errors.New("exit status 1"), false},
		{"exit status 2 (misuse)", errors.New("exit status 2"), false},
		{"exit status 127 (not found)", errors.New("exit status 127"), false},
		{"failed to start", errors.New("failed to start subprocess"), true},
		{"reading stdout", errors.New("reading stdout: broken pipe"), true},
		{"session idle for", errors.New("claude_code session idle for 90s"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isDelegateRetryable(c.err); got != c.want {
				t.Errorf("isDelegateRetryable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestExtractExitCode(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want int
	}{
		{"no prefix", "boom", -1},
		{"prefix only no digits", "exit status ", -1},
		{"simple", "exit status 1", 1},
		{"larger", "exit status 137", 137},
		{"with suffix", "subprocess: exit status 42 (SIGSEGV)", 42},
		{"two-digit boundary", "exit status 99", 99},
		{"three-digit", "exit status 200", 200},
		{"first match wins", "exit status 7 then exit status 99", 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractExitCode(c.msg); got != c.want {
				t.Errorf("extractExitCode(%q) = %d, want %d", c.msg, got, c.want)
			}
		})
	}
}

// newTestExecutorForRetry builds a minimal ClawExecutor sufficient for
// retryDelegateLoop. The retry policy uses a 1ms backoff base so the
// test stays sub-second even with multiple retries.
func newTestExecutorForRetry(maxAttempts int) *ClawExecutor {
	return &ClawExecutor{
		retry:  RetryPolicy{MaxAttempts: maxAttempts, BackoffBase: time.Millisecond},
		logger: iterlog.Nop(),
	}
}

func TestRetryDelegateLoop_SucceedsFirstTry(t *testing.T) {
	e := newTestExecutorForRetry(3)
	calls := 0
	want := delegate.Result{Output: map[string]any{"k": "v"}, BackendName: "claw"}
	got, err := e.retryDelegateLoop(context.Background(), "node1", "claw", func() (delegate.Result, error) {
		calls++
		return want, nil
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
	if got.Output["k"] != "v" {
		t.Errorf("output not propagated: %+v", got)
	}
}

func TestRetryDelegateLoop_RetriesOnTransientThenSucceeds(t *testing.T) {
	e := newTestExecutorForRetry(3)
	calls := 0
	got, err := e.retryDelegateLoop(context.Background(), "node1", "claw", func() (delegate.Result, error) {
		calls++
		if calls < 3 {
			return delegate.Result{}, &delegate.ErrTransient{Reason: "subprocess killed"}
		}
		return delegate.Result{Output: map[string]any{"ok": true}}, nil
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls (initial + 2 retries), got %d", calls)
	}
	if got.Output["ok"] != true {
		t.Errorf("expected success after retries: %+v", got)
	}
}

func TestRetryDelegateLoop_GivesUpAtMaxAttempts(t *testing.T) {
	e := newTestExecutorForRetry(3)
	calls := 0
	_, err := e.retryDelegateLoop(context.Background(), "node1", "claw", func() (delegate.Result, error) {
		calls++
		return delegate.Result{}, &delegate.ErrTransient{Reason: "subprocess killed"}
	})
	if err == nil {
		t.Fatal("expected error after max attempts")
	}
	if calls != 3 {
		t.Errorf("expected 3 calls total (1 initial + 2 retries up to maxAttempts=3), got %d", calls)
	}
}

func TestRetryDelegateLoop_NonRetryableErrorStopsImmediately(t *testing.T) {
	e := newTestExecutorForRetry(5)
	calls := 0
	_, err := e.retryDelegateLoop(context.Background(), "node1", "claw", func() (delegate.Result, error) {
		calls++
		return delegate.Result{}, errors.New("exit status 1") // application error, not retryable
	})
	if err == nil {
		t.Fatal("expected error to surface")
	}
	if calls != 1 {
		t.Errorf("expected 1 call for non-retryable error, got %d", calls)
	}
}

func TestRetryDelegateLoop_ContextCancelStopsBackoff(t *testing.T) {
	e := newTestExecutorForRetry(10)
	e.retry.BackoffBase = 500 * time.Millisecond // long enough to actually wait
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	// Cancel after the first call so we exit during the backoff sleep.
	_, err := e.retryDelegateLoop(ctx, "node1", "claw", func() (delegate.Result, error) {
		calls++
		if calls == 1 {
			cancel()
		}
		return delegate.Result{}, &delegate.ErrTransient{Reason: "subprocess killed"}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call before cancel, got %d", calls)
	}
}

func TestRetryDelegateLoop_OnDelegateRetryHookFires(t *testing.T) {
	e := newTestExecutorForRetry(3)
	var hookFires []DelegateInfo
	e.hooks.OnDelegateRetry = func(nodeID string, info DelegateInfo) {
		hookFires = append(hookFires, info)
	}
	calls := 0
	_, err := e.retryDelegateLoop(context.Background(), "node1", "claw", func() (delegate.Result, error) {
		calls++
		if calls < 3 {
			return delegate.Result{}, &delegate.ErrTransient{Reason: "boom"}
		}
		return delegate.Result{}, nil
	})
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(hookFires) != 2 {
		t.Errorf("expected 2 retry-hook fires (between 3 calls), got %d", len(hookFires))
	}
	for i, info := range hookFires {
		wantAttempt := i + 1
		if info.Attempt != wantAttempt {
			t.Errorf("hookFires[%d].Attempt = %d, want %d", i, info.Attempt, wantAttempt)
		}
		if info.BackendName != "claw" {
			t.Errorf("hookFires[%d].BackendName = %q, want claw", i, info.BackendName)
		}
	}
}
