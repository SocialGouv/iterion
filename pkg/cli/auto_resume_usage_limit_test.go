package cli

import (
	"errors"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/runtime/recovery"
)

func TestClassify_UsageWindowRoutesToBlockedCode(t *testing.T) {
	usage := &delegate.ErrRateLimited{
		Provider: "claude_code",
		Detail:   "You've hit your session limit · resets 10:30am",
		Kind:     delegate.RateLimitKindUsageWindow,
	}
	if code := recovery.Classify(usage); code != runtime.ErrCodeUsageLimitBlocked {
		t.Fatalf("Classify(usage window) = %s, want USAGE_LIMIT_BLOCKED", code)
	}
	throttle := &delegate.ErrRateLimited{Provider: "claude_code", Detail: "rate limit exceeded", Kind: delegate.RateLimitKindTransient}
	if code := recovery.Classify(throttle); code != runtime.ErrCodeRateLimited {
		t.Fatalf("Classify(throttle) = %s, want RATE_LIMITED", code)
	}
	// Legacy shape (no Kind) keeps the historical classification.
	legacy := &delegate.ErrRateLimited{Provider: "codex", Detail: "429"}
	if code := recovery.Classify(legacy); code != runtime.ErrCodeRateLimited {
		t.Fatalf("Classify(legacy) = %s, want RATE_LIMITED", code)
	}
}

func TestGateAutoResume_UsageLimitAllowed(t *testing.T) {
	gate := gateAutoResume(runtime.ErrCodeUsageLimitBlocked, autoResumeConfig{MaxAttempts: 3}, false)
	if !gate.proceed {
		t.Fatalf("gate = %+v, want proceed", gate)
	}
}

func TestUsageLimitDelay(t *testing.T) {
	now := time.Date(2026, 7, 17, 14, 0, 0, 0, time.UTC)

	t.Run("honors parsed reset with margin", func(t *testing.T) {
		err := wrapAsRuntime(&delegate.ErrRateLimited{
			Kind:    delegate.RateLimitKindUsageWindow,
			ResetAt: now.Add(90 * time.Minute),
		})
		got := usageLimitDelay(err, now)
		if got < 90*time.Minute || got > 92*time.Minute {
			t.Fatalf("delay = %s, want ~91m", got)
		}
	})

	t.Run("no hint falls back to window-scale wait", func(t *testing.T) {
		err := wrapAsRuntime(&delegate.ErrRateLimited{Kind: delegate.RateLimitKindUsageWindow})
		if got := usageLimitDelay(err, now); got != usageLimitFallbackDelay {
			t.Fatalf("delay = %s, want %s", got, usageLimitFallbackDelay)
		}
	})

	t.Run("past hint keeps the fallback floor", func(t *testing.T) {
		err := wrapAsRuntime(&delegate.ErrRateLimited{
			Kind:    delegate.RateLimitKindUsageWindow,
			ResetAt: now.Add(-time.Hour),
		})
		if got := usageLimitDelay(err, now); got != usageLimitFallbackDelay {
			t.Fatalf("delay = %s, want fallback", got)
		}
	})

	t.Run("mis-parsed far-future hint is capped", func(t *testing.T) {
		err := wrapAsRuntime(&delegate.ErrRateLimited{
			Kind:    delegate.RateLimitKindUsageWindow,
			ResetAt: now.Add(72 * time.Hour),
		})
		if got := usageLimitDelay(err, now); got != usageLimitMaxDelay {
			t.Fatalf("delay = %s, want cap %s", got, usageLimitMaxDelay)
		}
	})
}

// wrapAsRuntime mirrors how the executor surfaces backend errors: the
// delegate error rides RuntimeError.Cause, reachable via errors.As.
func wrapAsRuntime(cause error) error {
	return &runtime.RuntimeError{
		Code:    runtime.ErrCodeUsageLimitBlocked,
		Message: "usage window exhausted",
		Cause:   cause,
	}
}

func TestWrapAsRuntimeReachesCause(t *testing.T) {
	var rl *delegate.ErrRateLimited
	if !errors.As(wrapAsRuntime(&delegate.ErrRateLimited{Kind: delegate.RateLimitKindUsageWindow}), &rl) {
		t.Fatal("errors.As must traverse RuntimeError.Cause")
	}
}
