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

// TestRetryDelegateLoop_KeepsTheSpendItDiscards: the shape that hurts is the
// one where the LAST attempt is the cheap one — a session spends minutes, the
// retry cannot even spawn and reports nothing, and taking the last attempt's
// figure reports the cost of nothing. The caps, the org ledger and a lending
// donor all read the map, so the true figure has to survive.
func TestRetryDelegateLoop_KeepsTheSpendItDiscards(t *testing.T) {
	e := newTestExecutorForRetry(3)
	calls := 0
	billed := func(tokens int, usd float64) delegate.Result {
		return delegate.Result{
			Tokens:   tokens,
			Duration: time.Second,
			Output:   map[string]any{"_tokens": tokens, "_cost_usd": usd},
		}
	}
	got, err := e.retryDelegateLoop(context.Background(), "node1", "claw", false, func() (delegate.Result, error) {
		calls++
		if calls == 1 {
			// A whole session, then a transient wall.
			return billed(9000, 0.80), &delegate.ErrTransient{Reason: "stream closed"}
		}
		// The retry cannot even spawn: nothing billed, and it is terminal.
		return delegate.Result{}, errors.New("container is not running")
	})
	if err == nil || calls != 2 {
		t.Fatalf("want a failure after 2 attempts, got err=%v calls=%d", err, calls)
	}
	if got.Tokens != 9000 {
		t.Fatalf("the discarded attempt's tokens were lost: %d", got.Tokens)
	}
	if usd, _ := got.Output["_cost_usd"].(float64); usd < 0.80 {
		t.Fatalf("the discarded attempt's cost was lost: %v", got.Output["_cost_usd"])
	}
	if tok, _ := got.Output["_tokens"].(int); tok != 9000 {
		t.Fatalf("the map the caps read was not updated: %v", got.Output["_tokens"])
	}
	// The time is part of the same accounting: the event would otherwise
	// report the failed spawn's zero for a node that spent a whole second.
	if got.Duration != time.Second {
		t.Fatalf("the discarded attempt's duration was lost: %v", got.Duration)
	}
}

// The other direction, and the ONE case that folds at a MAX: two attempts
// CONTINUING one session, on a backend whose cost figure the provider
// itself computed over the whole session (claude_code's TotalCostUSD, the
// only shipped case — Result.CostIsSessionTotal says so). There attempt 2
// re-reports the running total, and summing would bill it twice.
//
// The tokens in the same reports are NOT cumulative — claude_code adds the
// resumed formatting pass's Usage onto pass 1's — so they still sum. Both
// halves are asserted here precisely because they disagree.
func TestRetryDelegateLoop_SessionTotalCostIsNotDoubled(t *testing.T) {
	e := newTestExecutorForRetry(3)
	calls := 0
	got, err := e.retryDelegateLoop(context.Background(), "node1", delegate.BackendClaudeCode, true, func() (delegate.Result, error) {
		calls++
		// Attempt 1 burns 1000 tokens for $0.10; attempt 2's own turn burns
		// 300 more, and the CLI reports the SESSION's $0.13.
		tokens, usd := 1000, 0.10
		if calls > 1 {
			tokens, usd = 300, 0.13
		}
		r := delegate.Result{
			Tokens:             tokens,
			CostIsSessionTotal: true,
			Output:             map[string]any{"_tokens": tokens, "_cost_usd": usd},
		}
		if calls < 2 {
			return r, &delegate.ErrTransient{Reason: "stream closed"}
		}
		return r, nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("want two attempts then success: err=%v calls=%d", err, calls)
	}
	if usd, _ := got.Output["_cost_usd"].(float64); usd != 0.13 {
		t.Fatalf("a session-cumulative cost was summed instead of taken at its max: %v", usd)
	}
	if got.Tokens != 1300 {
		t.Fatalf("per-call tokens were folded at their max instead of summed: %d", got.Tokens)
	}
}

// And the OTHER side of that max, which nothing exercised: the LAST report is
// the smaller one. `foldSpend` takes `usd = nc` and only then raises it to
// `pc`, so deleting the raise leaves every suite in pkg/backend/... and
// pkg/runtime green — measured 08/09 — while the run silently reports the
// cheaper of two readings of the same session.
//
// The shape is not hypothetical: a delegation that dies still names the
// session it opened and reports that session's cost AT THE MOMENT IT DIED. A
// first attempt that got far, then a retry that dies early, is exactly
// pc > nc — and taking the last figure bills the run for the shorter of the
// two. Same class as a failed node whose spend never reached the totals: the
// caps, the org ledger and a lending donor all read this map.
func TestRetryDelegateLoop_SessionTotalKeepsTheLargestReading(t *testing.T) {
	e := newTestExecutorForRetry(3)
	calls := 0
	got, err := e.retryDelegateLoop(context.Background(), "node1", delegate.BackendClaudeCode, true, func() (delegate.Result, error) {
		calls++
		// Attempt 1 gets far: the CLI reports the session at $0.13. Attempt 2
		// dies early and reports the same session at $0.05 — a smaller reading
		// of the very session that already cost $0.13.
		tokens, usd := 1000, 0.13
		if calls > 1 {
			tokens, usd = 300, 0.05
		}
		r := delegate.Result{
			Tokens:             tokens,
			CostIsSessionTotal: true,
			Output:             map[string]any{"_tokens": tokens, "_cost_usd": usd},
		}
		if calls < 2 {
			return r, &delegate.ErrTransient{Reason: "stream closed"}
		}
		return r, nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("want two attempts then success: err=%v calls=%d", err, calls)
	}
	if usd, _ := got.Output["_cost_usd"].(float64); usd != 0.13 {
		t.Fatalf("the run kept the LAST session reading instead of the largest: %v (want 0.13)", usd)
	}
	// Tokens are per-call and still sum: the max applies to the session-total
	// COST, never to the counts beside it.
	if got.Tokens != 1300 {
		t.Fatalf("per-call tokens were folded at their max instead of summed: %d", got.Tokens)
	}
}

// The same shared session, but the cost figure is an ESTIMATE derived from
// this call's tokens — claude_code under the OAuth forfait (the CLI reports
// no cost, AnnotateWithUSD degrades to Annotate), and every cost.Annotate
// backend besides. Nothing about it is cumulative, so it sums like the
// tokens it was computed from. A fold keyed on the session ALONE read this
// shape as cumulative and dropped a whole attempt's spend.
func TestRetryDelegateLoop_EstimatedCostSumsEvenInOneSession(t *testing.T) {
	e := newTestExecutorForRetry(3)
	calls := 0
	got, err := e.retryDelegateLoop(context.Background(), "node1", delegate.BackendClaudeCode, true, func() (delegate.Result, error) {
		calls++
		tokens, usd := 9000, 0.90
		if calls > 1 {
			tokens, usd = 1300, 0.13
		}
		r := delegate.Result{Tokens: tokens, Output: map[string]any{"_tokens": tokens, "_cost_usd": usd}}
		if calls < 2 {
			return r, &delegate.ErrTransient{Reason: "stream closed"}
		}
		return r, nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("want two attempts then success: err=%v calls=%d", err, calls)
	}
	if got.Tokens != 10300 {
		t.Fatalf("tokens = %d, want 10300 (every attempt billed its own turn)", got.Tokens)
	}
	if usd, _ := got.Output["_cost_usd"].(float64); usd < 1.02 {
		t.Fatalf("an estimated cost was folded at its max instead of summed: %v", usd)
	}
}

// A node executing for the FIRST time carries no session id, so the task
// the retry re-invokes with opens a fresh session every attempt and each
// report covers only its own work. Their spend is the SUM. Reading a MAX
// here reported 9000 for 10300 of work — an under-report, which is the
// direction that lets a run walk past max_cost_usd.
func TestRetryDelegateLoop_FreshSessionsEachSpendTheirOwn(t *testing.T) {
	e := newTestExecutorForRetry(3)
	calls := 0
	got, err := e.retryDelegateLoop(context.Background(), "node1", "claw", false, func() (delegate.Result, error) {
		calls++
		// Attempt 1 runs an agentic session; attempt 2 opens its own and
		// barely gets going. Neither report contains the other.
		tokens := 9000
		if calls > 1 {
			tokens = 1300
		}
		r := delegate.Result{Tokens: tokens, Output: map[string]any{
			"_tokens": tokens, "_cost_usd": float64(tokens) / 10000}}
		if calls < 2 {
			return r, &delegate.ErrTransient{Reason: "stream closed"}
		}
		return r, nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("want two attempts then success: err=%v calls=%d", err, calls)
	}
	if got.Tokens != 10300 {
		t.Fatalf("disjoint sessions were folded at their max instead of summed: %d", got.Tokens)
	}
	if tok, _ := got.Output["_tokens"].(int); tok != 10300 {
		t.Fatalf("the map the caps read was not updated: %v", got.Output["_tokens"])
	}
	if usd, _ := got.Output["_cost_usd"].(float64); usd < 1.03 {
		t.Fatalf("the cost the caps read was not summed: %v", usd)
	}
}

// A cancelled retry keeps the accounting too: no output survives a
// cancellation, but what the attempts spent is not undone by it.
func TestRetryDelegateLoop_CancelledKeepsTheSpend(t *testing.T) {
	e := newTestExecutorForRetry(3)
	ctx, cancel := context.WithCancel(context.Background())
	got, err := e.retryDelegateLoop(ctx, "node1", "claw", false, func() (delegate.Result, error) {
		cancel()
		return delegate.Result{Tokens: 500, Output: map[string]any{"_tokens": 500, "_cost_usd": 0.05}},
			&delegate.ErrTransient{Reason: "stream closed"}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want the cancellation, got %v", err)
	}
	if got.Tokens != 500 {
		t.Fatalf("a cancellation erased the spend: %+v", got)
	}
}

func TestRetryDelegateLoop_SucceedsFirstTry(t *testing.T) {
	e := newTestExecutorForRetry(3)
	calls := 0
	want := delegate.Result{Output: map[string]any{"k": "v"}, BackendName: "claw"}
	got, err := e.retryDelegateLoop(context.Background(), "node1", "claw", false, func() (delegate.Result, error) {
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
	got, err := e.retryDelegateLoop(context.Background(), "node1", "claw", false, func() (delegate.Result, error) {
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
	_, err := e.retryDelegateLoop(context.Background(), "node1", "claw", false, func() (delegate.Result, error) {
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
	_, err := e.retryDelegateLoop(context.Background(), "node1", "claw", false, func() (delegate.Result, error) {
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
	_, err := e.retryDelegateLoop(ctx, "node1", "claw", false, func() (delegate.Result, error) {
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
	_, err := e.retryDelegateLoop(context.Background(), "node1", "claw", false, func() (delegate.Result, error) {
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
