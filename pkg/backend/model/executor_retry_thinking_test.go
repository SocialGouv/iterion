package model

import (
	"context"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

// Thinking is a per-attempt count like the tokens it is part of, so the
// attempts of a node SUM (#3283, F12). Measured on a review-pr reviewer:
// 185,010 thinking tokens on the checkpoint against 463,458 in the step
// events — the later attempt's figure was kept and the earlier ones
// dropped. Each case drives the executor's retry loop, whose fold every
// retry of a node goes through (claw's own loop folds the same way), and
// reads the `_thinking_tokens` / `_thinking_ms` the node output is stamped
// with. The attempts run in both orders, the one without usage first, in
// the middle and last.
//
// Mutant: keep the later attempt's thinking in foldSpend (no sum) and every
// case reports the last attempt's figure — 0 when it is the one without
// usage.
func TestFoldSpendSumsThinkingAcrossAttempts(t *testing.T) {
	t.Parallel()
	billed := func(tokens, thinking, ms int) delegate.Result {
		return delegate.Result{Tokens: tokens, ThinkingTokens: thinking, ThinkingMs: ms,
			Output: map[string]any{"_tokens": tokens}}
	}
	// Each case builds its OWN attempts: the fold stamps the summed spend into
	// the latest attempt's Output map, which the retry loop owns, so attempts
	// shared across these parallel subtests were one map written by two
	// goroutines at once (#3384).
	a := func() delegate.Result { return billed(5000, 400, 900) }
	b := func() delegate.Result { return billed(2000, 150, 300) }
	noUsage := func() delegate.Result { return delegate.Result{} }
	for _, tc := range []struct {
		name     string
		attempts func() []delegate.Result
	}{
		{"the attempt without usage last", func() []delegate.Result { return []delegate.Result{a(), b(), noUsage()} }},
		{"the attempt without usage first", func() []delegate.Result { return []delegate.Result{noUsage(), b(), a()} }},
		{"the attempt without usage in the middle", func() []delegate.Result { return []delegate.Result{b(), noUsage(), a()} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			attempts := tc.attempts()
			e := newTestExecutorForRetry(len(attempts))
			calls := 0
			got, err := e.retryDelegateLoop(context.Background(), "reviewer", delegate.BackendClaudeCode, false,
				func() (delegate.Result, error) {
					r := attempts[calls]
					calls++
					if calls < len(attempts) {
						return r, &delegate.ErrTransient{Reason: "stream closed"}
					}
					return r, nil
				})
			if err != nil || calls != len(attempts) {
				t.Fatalf("want every attempt to run: err=%v calls=%d", err, calls)
			}
			if got.ThinkingTokens != 550 || got.ThinkingMs != 1200 {
				t.Fatalf("thinking = %d tokens / %d ms, want the attempts' 550 / 1200", got.ThinkingTokens, got.ThinkingMs)
			}
			out := map[string]any{}
			stampDelegateOutputMeta(out, got, delegate.BackendClaudeCode)
			if out["_thinking_tokens"] != 550 || out["_thinking_ms"] != 1200 {
				t.Errorf("stamped _thinking_tokens=%v _thinking_ms=%v, want 550 and 1200", out["_thinking_tokens"], out["_thinking_ms"])
			}
		})
	}
}

// A route the chain abandons thought too: its thinking joins the serving
// route's, like its tokens (chainSpend).
//
// Mutant: drop the thinking from chainSpend.applyTo and the serving route's
// figure is all that remains.
func TestChainSpendSumsThinkingAcrossRoutes(t *testing.T) {
	t.Parallel()
	var spent chainSpend
	spent.add(delegate.Result{Tokens: 3000, ThinkingTokens: 700, ThinkingMs: 2000}, spendLabel{backend: delegate.BackendClaw})
	spent.add(delegate.Result{}, spendLabel{backend: delegate.BackendClaw})
	got := spent.applyTo(delegate.Result{Tokens: 1000, ThinkingTokens: 100, ThinkingMs: 250, Output: map[string]any{}})
	if got.ThinkingTokens != 800 || got.ThinkingMs != 2250 {
		t.Fatalf("thinking = %d tokens / %d ms, want the routes' 800 / 2250", got.ThinkingTokens, got.ThinkingMs)
	}
	// Thinking alone still folds: an abandoned route that reported thinking
	// and nothing else must not be dropped by applyTo's empty-spend shortcut.
	var thinkingOnly chainSpend
	thinkingOnly.add(delegate.Result{ThinkingTokens: 40, ThinkingMs: 90}, spendLabel{backend: delegate.BackendClaw})
	if got := thinkingOnly.applyTo(delegate.Result{}); got.ThinkingTokens != 40 || got.ThinkingMs != 90 {
		t.Errorf("thinking-only route folded to %d / %d, want 40 / 90", got.ThinkingTokens, got.ThinkingMs)
	}
}
