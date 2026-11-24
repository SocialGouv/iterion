package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type classifiedAmbiguousContext struct{ error }

func (e classifiedAmbiguousContext) Unwrap() error         { return e.error }
func (e classifiedAmbiguousContext) AmbiguousEffect() bool { return true }
func TestSimpleContextCauseRetainsClassifiedErrors(t *testing.T) {
	t.Parallel()
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		cases := []struct {
			name string
			err  error
			want bool
		}{
			{"direct", cause, true}, {"fmt", fmt.Errorf("wait: %w", cause), true},
			{"neutralRuntime", &RuntimeError{Code: ErrCodeExecutionFailed, Cause: cause}, true},
			{"businessRuntime", &RuntimeError{Code: ErrCodeBudgetExceeded, Cause: cause}, false},
			{"neutralAboveBusiness", &RuntimeError{Code: ErrCodeExecutionFailed, Cause: &RuntimeError{Code: ErrCodeBudgetExceeded, Cause: cause}}, false},
			{"ambiguousRuntime", &RuntimeError{Code: ErrCodeAmbiguousEffect, Cause: cause}, false},
			{"ambiguousCustom", classifiedAmbiguousContext{cause}, false},
			{"unrelatedJoined", errors.Join(cause, errors.New("business failure")), false},
		}
		for _, tt := range cases {
			t.Run(fmt.Sprint(cause)+"/"+tt.name, func(t *testing.T) {
				if got := simpleContextCause(tt.err, cause); got != tt.want {
					t.Fatalf("classification=%v want=%v: %v", got, tt.want, tt.err)
				}
			})
		}
	}
}
