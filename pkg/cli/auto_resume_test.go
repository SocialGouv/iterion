package cli

import (
	"errors"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/runtime"
)

func TestResolveAutoResume(t *testing.T) {
	t.Run("flag wins over env", func(t *testing.T) {
		t.Setenv("ITERION_AUTO_RESUME", "2")
		cfg := resolveAutoResume(5, BudgetOverrides{}, retrypolicy.Policy{})
		if cfg.MaxAttempts != 5 {
			t.Errorf("MaxAttempts = %d, want 5", cfg.MaxAttempts)
		}
	})
	t.Run("env fallback when flag 0", func(t *testing.T) {
		t.Setenv("ITERION_AUTO_RESUME", "3")
		cfg := resolveAutoResume(0, BudgetOverrides{}, retrypolicy.Policy{})
		if cfg.MaxAttempts != 3 {
			t.Errorf("MaxAttempts = %d, want 3", cfg.MaxAttempts)
		}
	})
	t.Run("default off", func(t *testing.T) {
		t.Setenv("ITERION_AUTO_RESUME", "")
		cfg := resolveAutoResume(0, BudgetOverrides{}, retrypolicy.Policy{})
		if cfg.MaxAttempts != 0 {
			t.Errorf("MaxAttempts = %d, want 0 (off)", cfg.MaxAttempts)
		}
	})
	t.Run("budgetRaised reflects --max-* flags", func(t *testing.T) {
		if resolveAutoResume(1, BudgetOverrides{}, retrypolicy.Policy{}).BudgetRaised {
			t.Error("BudgetRaised should be false with no overrides")
		}
		if !resolveAutoResume(1, BudgetOverrides{MaxDuration: "4h"}, retrypolicy.Policy{}).BudgetRaised {
			t.Error("BudgetRaised should be true with --max-duration")
		}
	})
}

func TestGateAutoResume(t *testing.T) {
	raised := autoResumeConfig{MaxAttempts: 3, BudgetRaised: true}
	notRaised := autoResumeConfig{MaxAttempts: 3, BudgetRaised: false}

	cases := []struct {
		name          string
		code          runtime.ErrorCode
		cfg           autoResumeConfig
		budgetResumed bool
		wantProceed   bool
		wantConsume   bool
	}{
		// retryable classes
		{"execution failed", runtime.ErrCodeExecutionFailed, notRaised, false, true, false},
		{"timeout", runtime.ErrCodeTimeout, notRaised, false, true, false},
		{"rate limited", runtime.ErrCodeRateLimited, notRaised, false, true, false},
		{"network transient", runtime.ErrCodeNetworkTransient, notRaised, false, true, false},
		{"tool transient", runtime.ErrCodeToolFailedTransient, notRaised, false, true, false},
		// terminal classes — must NOT auto-resume
		{"schema validation", runtime.ErrCodeSchemaValidation, notRaised, false, false, false},
		{"auth failed", runtime.ErrCodeAuthFailed, notRaised, false, false, false},
		{"workspace safety", runtime.ErrCodeWorkspaceSafety, notRaised, false, false, false},
		{"loop exhausted", runtime.ErrCodeLoopExhausted, notRaised, false, false, false},
		{"tool permanent", runtime.ErrCodeToolFailedPermanent, notRaised, false, false, false},
		{"unclassified", runtime.ErrorCode(""), notRaised, false, false, false},
		// A bot-defined code from a `resumable: true` fail node. The
		// allow-list is closed, so it lands here by construction — and
		// that is the RIGHT default: the run refused deliberately, and the
		// only thing that can change the verdict is an operator changing
		// something (a raised cap, a `--var`). Auto-resuming would replay
		// the same refusal on a loop.
		{"typed fail node code", runtime.ErrorCode("PLAN_BUDGET_EXHAUSTED"), notRaised, false, false, false},
		{"typed fail node code, cap raised", runtime.ErrorCode("PLAN_BUDGET_EXHAUSTED"), raised, false, false, false},
		{"engine fail-node constant", runtime.ErrorCode("FAIL_NODE"), notRaised, false, false, false},
		// budget special-casing
		{"budget without raised cap → stop", runtime.ErrCodeBudgetExceeded, notRaised, false, false, false},
		{"budget with raised cap → proceed once", runtime.ErrCodeBudgetExceeded, raised, false, true, true},
		{"budget with raised cap, already resumed → stop", runtime.ErrCodeBudgetExceeded, raised, true, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := gateAutoResume(c.code, c.cfg, c.budgetResumed)
			if g.proceed != c.wantProceed {
				t.Errorf("proceed = %v, want %v (reason: %q)", g.proceed, c.wantProceed, g.reason)
			}
			if g.consumeBudget != c.wantConsume {
				t.Errorf("consumeBudget = %v, want %v", g.consumeBudget, c.wantConsume)
			}
			if !g.proceed && g.reason == "" {
				t.Error("a stop decision must carry a reason")
			}
		})
	}
}

func TestRuntimeCode(t *testing.T) {
	re := &runtime.RuntimeError{Code: runtime.ErrCodeBudgetExceeded, Message: "over budget"}
	if got := runtimeCode(re); got != runtime.ErrCodeBudgetExceeded {
		t.Errorf("runtimeCode = %q, want BUDGET_EXCEEDED", got)
	}
	if got := runtimeCode(errors.New("plain")); got != "" {
		t.Errorf("runtimeCode(plain) = %q, want empty", got)
	}
	// wrapped RuntimeError is still unwrapped
	wrapped := &runtime.RuntimeError{Code: runtime.ErrCodeExecutionFailed, Cause: errors.New("boom")}
	if got := runtimeCode(wrapped); got != runtime.ErrCodeExecutionFailed {
		t.Errorf("runtimeCode(wrapped) = %q, want EXECUTION_FAILED", got)
	}
}

func TestAutoResumeBackoff(t *testing.T) {
	// Monotonic-in-expectation and capped. Jitter is 0.5x–1.5x, so attempt 0
	// is in [15s,45s] and the cap bounds the high end.
	d0 := autoResumeBackoff(0)
	if d0 < 15*time.Second || d0 > 45*time.Second {
		t.Errorf("attempt 0 backoff = %s, want within [15s,45s]", d0)
	}
	// A large attempt must never exceed the cap * 1.5 jitter ceiling.
	big := autoResumeBackoff(20)
	if big > time.Duration(float64(autoResumeBackoffCap)*1.5) {
		t.Errorf("capped backoff = %s exceeds cap*1.5", big)
	}
}
