package recovery

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"

	"github.com/SocialGouv/iterion/pkg/runtime"
)

func TestRateLimitRecipe_RetriesThenPauses(t *testing.T) {
	r := RateLimitRecipe(2)
	for i := 0; i < 2; i++ {
		act := r.Apply(context.Background(), &runtime.RuntimeError{Code: runtime.ErrCodeRateLimited}, i)
		if act.Kind != ActionRetrySameNode {
			t.Fatalf("attempt %d: expected RetrySameNode, got %v", i, act.Kind)
		}
		if act.Delay <= 0 {
			t.Errorf("attempt %d: expected positive delay, got %v", i, act.Delay)
		}
	}
	act := r.Apply(context.Background(), &runtime.RuntimeError{Code: runtime.ErrCodeRateLimited}, 2)
	if act.Kind != ActionPauseForHuman {
		t.Errorf("after retries exhausted: expected PauseForHuman, got %v", act.Kind)
	}
}

func TestContextLengthRecipe_CompactThenFail(t *testing.T) {
	r := ContextLengthRecipe()
	for i := 0; i < 2; i++ {
		act := r.Apply(context.Background(), &runtime.RuntimeError{Code: runtime.ErrCodeContextLengthExceeded}, i)
		if act.Kind != ActionCompactAndRetry {
			t.Fatalf("attempt %d: expected CompactAndRetry, got %v", i, act.Kind)
		}
	}
	act := r.Apply(context.Background(), &runtime.RuntimeError{Code: runtime.ErrCodeContextLengthExceeded}, 2)
	if act.Kind != ActionFailTerminal {
		t.Errorf("after compaction failed twice: expected FailTerminal, got %v", act.Kind)
	}
}

func TestBudgetRecipe_AlwaysPauses(t *testing.T) {
	r := BudgetRecipe()
	for i := 0; i < 5; i++ {
		act := r.Apply(context.Background(), &runtime.RuntimeError{Code: runtime.ErrCodeBudgetExceeded}, i)
		if act.Kind != ActionPauseForHuman {
			t.Fatalf("attempt %d: expected PauseForHuman, got %v", i, act.Kind)
		}
	}
}

func TestTransientToolRecipe(t *testing.T) {
	r := TransientToolRecipe(2)
	for i := 0; i < 2; i++ {
		act := r.Apply(context.Background(), &runtime.RuntimeError{Code: runtime.ErrCodeToolFailedTransient}, i)
		if act.Kind != ActionRetrySameNode {
			t.Fatalf("attempt %d: expected RetrySameNode, got %v", i, act.Kind)
		}
	}
	act := r.Apply(context.Background(), &runtime.RuntimeError{Code: runtime.ErrCodeToolFailedTransient}, 2)
	if act.Kind != ActionFailTerminal {
		t.Errorf("after retries exhausted: expected FailTerminal, got %v", act.Kind)
	}
}

func TestPermanentToolRecipe(t *testing.T) {
	r := PermanentToolRecipe()
	act := r.Apply(context.Background(), &runtime.RuntimeError{Code: runtime.ErrCodeToolFailedPermanent}, 0)
	if act.Kind != ActionFailTerminal {
		t.Errorf("expected FailTerminal, got %v", act.Kind)
	}
}

func TestClassify_RuntimeError(t *testing.T) {
	rerr := &runtime.RuntimeError{Code: runtime.ErrCodeBudgetExceeded}
	if got := Classify(rerr); got != runtime.ErrCodeBudgetExceeded {
		t.Errorf("expected BUDGET_EXCEEDED, got %v", got)
	}
}

func TestClassify_RateLimit429(t *testing.T) {
	apiErr := &api.APIError{StatusCode: 429, Message: "Too many requests"}
	if got := Classify(apiErr); got != runtime.ErrCodeRateLimited {
		t.Errorf("expected RATE_LIMITED, got %v", got)
	}
}

func TestClassify_ContextLengthFromMessage(t *testing.T) {
	apiErr := &api.APIError{StatusCode: 400, Message: "context_length_exceeded: 200000 tokens"}
	if got := Classify(apiErr); got != runtime.ErrCodeContextLengthExceeded {
		t.Errorf("expected CONTEXT_LENGTH_EXCEEDED, got %v", got)
	}
}

func TestClassify_GenericAPIError(t *testing.T) {
	apiErr := &api.APIError{StatusCode: 500, Message: "internal error"}
	if got := Classify(apiErr); got != runtime.ErrCodeExecutionFailed {
		t.Errorf("expected EXECUTION_FAILED, got %v", got)
	}
}

func TestClassify_PlainError(t *testing.T) {
	if got := Classify(errors.New("something")); got != runtime.ErrCodeExecutionFailed {
		t.Errorf("expected EXECUTION_FAILED for plain error, got %v", got)
	}
}

func TestClassify_Nil(t *testing.T) {
	if got := Classify(nil); got != "" {
		t.Errorf("expected empty for nil err, got %v", got)
	}
}

func TestDefaultRecipes_HasAllClasses(t *testing.T) {
	r := DefaultRecipes()
	want := []runtime.ErrorCode{
		runtime.ErrCodeRateLimited,
		runtime.ErrCodeContextLengthExceeded,
		runtime.ErrCodeBudgetExceeded,
		runtime.ErrCodeToolFailedTransient,
		runtime.ErrCodeToolFailedPermanent,
	}
	for _, code := range want {
		if _, ok := r[code]; !ok {
			t.Errorf("DefaultRecipes missing %q", code)
		}
	}
}
