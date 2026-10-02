package runtime

import (
	"errors"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

// errorCode reads a RuntimeError's Code, and also recognises the
// ErrBudgetExceeded sentinel so a %w-wrapped budget exceedance (what
// checkPreExecBudget writes, not a typed RuntimeError) reads on the
// event with the same code the trunk's storage layer stamps —
// FailureBudgetExceeded / BUDGET_EXCEEDED. Without the recognition a
// budget-exceeded branch would file as a dead branch and Clean() would
// contradict "a budget ceiling is not a death" (PR #1491 review Rdabb2b).
func TestErrorCodeReadsBudgetSentinelAndRuntimeError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{
			"plain %w-wrapped ErrBudgetExceeded, as checkPreExecBudget writes",
			fmt.Errorf("%w: iterations (3/3)", ErrBudgetExceeded),
			string(store.FailureBudgetExceeded),
		},
		{
			"deep %w chain wrapping ErrBudgetExceeded",
			fmt.Errorf("branch: %w", fmt.Errorf("preflight: %w", ErrBudgetExceeded)),
			string(store.FailureBudgetExceeded),
		},
		{
			"RuntimeError with a Code — wins over the sentinel check",
			&RuntimeError{Code: ErrCodeExpressionFailed, Message: "expr"},
			string(ErrCodeExpressionFailed),
		},
		{
			"RuntimeError with ErrBudgetExceeded as Cause",
			&RuntimeError{Code: ErrCodeBudgetExceeded, Cause: ErrBudgetExceeded, Message: "budget"},
			string(ErrCodeBudgetExceeded),
		},
		{"plain non-typed error carries no code", errors.New("something went wrong"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorCode(tc.err); got != tc.want {
				t.Errorf("errorCode(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// declinedReason returns the LoopDeclined.Reason an error wraps: what
// the branch's event carries so dryrun's classifier reads a ceiling
// reason the same way the trunk's ceilingOf does.
func TestDeclinedReasonReadsLoopDeclined(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"plain error, no decline", errors.New("boom"), ""},
		{
			"LoopDeclined direct",
			&LoopDeclined{Loop: "fix", Reason: "loop_cap"},
			"loop_cap",
		},
		{
			"RuntimeError with LoopDeclined as Cause",
			&RuntimeError{Code: ErrCodeLoopExhausted, Cause: &LoopDeclined{Loop: "fix", Reason: "liveness_stall"}},
			"liveness_stall",
		},
		{
			"deep chain wrapping a LoopDeclined",
			fmt.Errorf("branch: %w", &RuntimeError{Code: ErrCodeNoOutgoingEdge, Cause: &LoopDeclined{Loop: "fix", Reason: "dry_run_crossings"}}),
			"dry_run_crossings",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := declinedReason(tc.err); got != tc.want {
				t.Errorf("declinedReason(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
