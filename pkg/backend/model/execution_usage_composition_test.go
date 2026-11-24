package model

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"testing"
)

func TestExecutionUsageCompositionEveryOutcome(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name               string
		out                map[string]any
		cause              error
		tokens, unreported int
	}{
		{"valid", map[string]any{"verdict": true, "reason": "done"}, nil, 7, 0},
		{"ineligible", map[string]any{"verdict": "wrong", "reason": "done"}, nil, 7, 0},
		{"missing budget zero", map[string]any{"verdict": true}, nil, 7, 0},
		{"error", nil, errors.New("terminal failure"), 7, 0},
		{"cancel", nil, context.Canceled, 7, 0},
		{"no answer unreported", nil, &delegate.ErrAuthFailed{}, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			c := &ExecutionUsageCollector{}
			e := producerExecutor(persistentRouteBackend(func(context.Context, delegate.Task) (delegate.Result, error) {
				calls++
				return delegate.Result{Output: copyCorrectionOutput(tc.out), Tokens: tc.tokens, CostUSD: 0, UnreportedCalls: tc.unreported}, tc.cause
			}))
			ctx := WithExecutionUsageCollector(producerOwnedContext(), c)
			_, err := e.Execute(ctx, verdictJudge(delegate.BackendClaudeCode), nil)
			var f *OutputCorrectionFailure
			if errors.As(err, &f) {
				f.Handle().Release()
			}
			s, se := c.Snapshot()
			if se != nil || calls != 1 || s.Tokens != tc.tokens || s.UnpricedTokens != tc.tokens || s.UnreportedCalls != tc.unreported {
				t.Fatalf("calls=%d facts=%+v snapshoterr=%v executeerr=%v", calls, s, se, err)
			}
		})
	}
}
func TestExecutionUsageCompositionRounds(t *testing.T) {
	t.Parallel()
	calls := 0
	c := &ExecutionUsageCollector{}
	e := producerExecutor(persistentRouteBackend(func(context.Context, delegate.Task) (delegate.Result, error) {
		calls++
		out := map[string]any{"verdict": true}
		if calls == 3 {
			out["reason"] = "done"
		}
		return delegate.Result{Output: out, Tokens: 5, CostUSD: float64(10 + calls), CostIsSessionTotal: true, SessionID: "s", SessionFingerprint: "fp"}, nil
	}))
	ctx := WithExecutionUsageCollector(producerOwnedContext(), c)
	node := verdictJudge(delegate.BackendClaudeCode)
	f, ctx := requireProducerFailure(t, e, ctx, node)
	first, err := c.Snapshot()
	if err != nil || first.Tokens != 5 || len(first.Sessions) != 1 || first.Sessions[0].CostUSD != 11 || first.Sessions[0].Backend != delegate.BackendClaudeCode {
		t.Fatalf("first=%+v %v", first, err)
	}
	for want := 12; want <= 13; want++ {
		_, _, err = e.CorrectOutputWithUsage(ctx, node, f.Output(), f.validation)
		if err != nil {
			t.Fatal(err)
		}
		s, err := c.Since(first)
		if err != nil || s.Tokens != 5*(want-11) || len(s.Sessions) != 1 || s.Sessions[0].CostUSD != float64(want) || s.UnpricedTokens != 0 || s.OtherCostUSD != 0 {
			t.Fatalf("round=%+v %v", s, err)
		}
	}
}
func TestExecutionUsageCompositionCollectorOwnedByHandle(t *testing.T) {
	t.Parallel()
	calls := 0
	e := producerExecutor(persistentRouteBackend(func(context.Context, delegate.Task) (delegate.Result, error) {
		calls++
		return delegate.Result{Output: map[string]any{"verdict": true}, Tokens: 1}, nil
	}))
	c := &ExecutionUsageCollector{}
	node := verdictJudge(delegate.BackendClaudeCode)
	f, ctx := requireProducerFailure(t, e, WithExecutionUsageCollector(producerOwnedContext(), c), node)
	_, _, err := e.CorrectOutputWithUsage(WithExecutionUsageCollector(ctx, &ExecutionUsageCollector{}), node, f.Output(), f.validation)
	if !errors.Is(err, ErrOutputCorrectionIneligible) || calls != 1 {
		t.Fatalf("collector substitution accepted: calls=%d err=%v", calls, err)
	}
}
