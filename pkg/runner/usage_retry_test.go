package runner

import (
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/store"
)

var retryNow = time.Date(2026, 7, 27, 6, 0, 0, 0, time.UTC)

// noJitter isolates the instant arithmetic from the spread. Jitter has its
// own test; mixing them would force every expectation into a range.
func noJitter(p retrypolicy.Policy) retrypolicy.Policy {
	p.Jitter = "0s"
	return retrypolicy.Normalize(p)
}

func weeklyWindowErr(resetAt time.Time) error {
	return &delegate.ErrRateLimited{
		Provider: delegate.BackendClaudeCode,
		Detail:   "You've hit your weekly limit · resets Jul 28, 9pm (UTC)",
		Kind:     delegate.RateLimitKindUsageWindow,
		ResetAt:  resetAt,
	}
}

func TestUsageWindowRetryAt(t *testing.T) {
	reset := time.Date(2026, 7, 28, 21, 0, 0, 0, time.UTC)

	tests := []struct {
		name       string
		err        error
		pol        retrypolicy.Policy
		wantOK     bool
		wantAt     time.Time
		wantSource string
	}{
		{
			name:       "typed error carries the reset instant",
			err:        weeklyWindowErr(reset),
			pol:        noJitter(retrypolicy.Policy{}),
			wantOK:     true,
			wantAt:     reset.Add(time.Minute),
			wantSource: "typed_error",
		},
		{
			// What a real failure looks like once the engine has wrapped it:
			// a RuntimeError carrying the typed cause.
			name: "typed cause survives a RuntimeError wrapper",
			err: &runtime.RuntimeError{
				Code:    runtime.ErrCodeUsageLimitBlocked,
				Message: `node "synthesize" execution failed`,
				Cause:   weeklyWindowErr(reset),
			},
			pol:        noJitter(retrypolicy.Policy{}),
			wantOK:     true,
			wantAt:     reset.Add(time.Minute),
			wantSource: "typed_error",
		},
		{
			name:       "wrapped with %w still resolves",
			err:        fmt.Errorf("branch failed: %w", weeklyWindowErr(reset)),
			pol:        noJitter(retrypolicy.Policy{}),
			wantOK:     true,
			wantAt:     reset.Add(time.Minute),
			wantSource: "typed_error",
		},
		{
			// The classified-but-flattened case: no typed cause left, but the
			// message still holds the notice. This is the fallback that keeps
			// the feature working on a host whose error plumbing lost the type.
			name: "code only, instant recovered from the message",
			err: &runtime.RuntimeError{
				Code:    runtime.ErrCodeUsageLimitBlocked,
				Message: `node "synthesize" execution failed: rate_limited (claude_code): You've hit your weekly limit · resets Jul 28, 9pm (UTC)`,
			},
			pol:        noJitter(retrypolicy.Policy{}),
			wantOK:     true,
			wantAt:     reset,
			wantSource: "runtime_code+parsed_text",
		},
		{
			name: "usage window with no parseable instant still retries, bounded",
			err: &runtime.RuntimeError{
				Code:    runtime.ErrCodeUsageLimitBlocked,
				Message: "quota exhausted, no further detail",
			},
			pol:        noJitter(retrypolicy.Policy{}),
			wantOK:     true,
			wantAt:     retryNow.Add(usageWindowBlindWait),
			wantSource: "runtime_code+blind_wait",
		},
		{
			// A reset already behind us (or a timezone the parser read as
			// UTC) must not schedule a retry in the past.
			name:       "past instant is floored",
			err:        weeklyWindowErr(retryNow.Add(-3 * time.Hour)),
			pol:        noJitter(retrypolicy.Policy{}),
			wantOK:     true,
			wantAt:     retryNow.Add(usageWindowFloor),
			wantSource: "typed_error",
		},
		{
			name:       "instant beyond max_wait is clamped",
			err:        weeklyWindowErr(retryNow.Add(30 * 24 * time.Hour)),
			pol:        noJitter(retrypolicy.Policy{MaxWait: "36h"}),
			wantOK:     true,
			wantAt:     retryNow.Add(36 * time.Hour),
			wantSource: "typed_error",
		},
		{
			name:   "a transient throttle is not a usage window",
			err:    &delegate.ErrRateLimited{Kind: delegate.RateLimitKindTransient, Detail: "slow down"},
			pol:    noJitter(retrypolicy.Policy{}),
			wantOK: false,
		},
		{
			name:   "an unrelated failure is not retried",
			err:    &runtime.RuntimeError{Code: runtime.ErrCodeExecutionFailed, Message: "tool blew up"},
			pol:    noJitter(retrypolicy.Policy{}),
			wantOK: false,
		},
		{
			name:   "no error",
			err:    nil,
			pol:    noJitter(retrypolicy.Policy{}),
			wantOK: false,
		},
		{
			// The opt-out has to be a real opt-out: policy off means the run
			// falls back to today's redelivery behaviour untouched.
			name:   "policy off disables the carve-out entirely",
			err:    weeklyWindowErr(reset),
			pol:    noJitter(retrypolicy.Policy{UsageWindow: retrypolicy.UsageWindowOff}),
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at, source, ok := usageWindowRetryAt(tt.err, tt.pol, retryNow)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if !at.Equal(tt.wantAt) {
				t.Errorf("at = %v, want %v", at, tt.wantAt)
			}
			if source != tt.wantSource {
				t.Errorf("source = %q, want %q", source, tt.wantSource)
			}
		})
	}
}

// TestUsageWindowRetryAt_JitterStaysInBand pins the spread: several runs
// dying on one reset must not all come back at the same instant, and the
// spread must stay inside the configured band.
func TestUsageWindowRetryAt_JitterStaysInBand(t *testing.T) {
	reset := retryNow.Add(30 * time.Hour)
	pol := retrypolicy.Normalize(retrypolicy.Policy{Jitter: "10m"})
	base := reset.Add(time.Minute)

	seen := map[time.Time]bool{}
	for i := 0; i < 200; i++ {
		at, _, ok := usageWindowRetryAt(weeklyWindowErr(reset), pol, retryNow)
		if !ok {
			t.Fatal("ok = false")
		}
		if at.Before(base) || !at.Before(base.Add(10*time.Minute)) {
			t.Fatalf("at = %v, want within [%v, %v)", at, base, base.Add(10*time.Minute))
		}
		seen[at] = true
	}
	if len(seen) < 2 {
		t.Error("jitter produced a single instant across 200 draws — runs sharing a reset would stampede")
	}
}

// TestUsageWindowRetryAt_JitterNeverPushesPastMaxWait pins the ordering of
// the clamps: the spread is applied first, the ceiling last, so jitter can
// never carry a retry beyond the operator's declared horizon.
func TestUsageWindowRetryAt_JitterNeverPushesPastMaxWait(t *testing.T) {
	pol := retrypolicy.Normalize(retrypolicy.Policy{MaxWait: "2h", Jitter: "30m"})
	ceiling := retryNow.Add(2 * time.Hour)
	for i := 0; i < 100; i++ {
		at, _, ok := usageWindowRetryAt(weeklyWindowErr(retryNow.Add(90*time.Minute)), pol, retryNow)
		if !ok {
			t.Fatal("ok = false")
		}
		if at.After(ceiling) {
			t.Fatalf("at = %v exceeds max_wait ceiling %v", at, ceiling)
		}
	}
}

// TestRunRetryPolicy_ReadsTheLaunchSnapshot pins that the runner takes the
// policy resolved at launch and never re-derives it — it has no access to
// schedules or manifests, by design.
func TestRunRetryPolicy_ReadsTheLaunchSnapshot(t *testing.T) {
	got := runRetryPolicy(&store.Run{RetryPolicy: &store.RunRetryPolicy{
		UsageWindow: retrypolicy.UsageWindowResume,
		MaxAttempts: 2,
		MaxWait:     "36h",
		Jitter:      "1m",
	}})
	if got.MaxAttempts != 2 || got.MaxWaitDuration() != 36*time.Hour || got.JitterDuration() != time.Minute {
		t.Errorf("policy = %+v, want the snapshot's values", got)
	}
}

func TestRunRetryPolicy_NilSnapshotFallsBackToDefaults(t *testing.T) {
	// Runs launched before the snapshot existed, and surfaces that do not
	// resolve a policy, must still retry rather than silently opt out.
	got := runRetryPolicy(&store.Run{})
	if !got.Enabled() {
		t.Error("a run with no policy snapshot must still be retryable")
	}
	if got.MaxAttempts != retrypolicy.DefaultMaxAttempts {
		t.Errorf("max_attempts = %d, want the default %d", got.MaxAttempts, retrypolicy.DefaultMaxAttempts)
	}
	if runRetryPolicy(nil).MaxAttempts != retrypolicy.DefaultMaxAttempts {
		t.Error("a nil run must not panic and must yield the defaults")
	}
}

// TestRunRetryPolicy_PlatformCeilingLowers pins that a tenant cannot exceed
// the platform bound by declaring a bigger one on their own schedule.
func TestRunRetryPolicy_PlatformCeilingLowers(t *testing.T) {
	t.Setenv(retrypolicy.EnvCeilingMaxAttempts, "2")
	t.Setenv(retrypolicy.EnvCeilingMaxWait, "6h")

	got := runRetryPolicy(&store.Run{RetryPolicy: &store.RunRetryPolicy{MaxAttempts: 50, MaxWait: "720h"}})
	if got.MaxAttempts != 2 {
		t.Errorf("max_attempts = %d, want 2 (platform ceiling)", got.MaxAttempts)
	}
	if got.MaxWaitDuration() != 6*time.Hour {
		t.Errorf("max_wait = %v, want 6h (platform ceiling)", got.MaxWaitDuration())
	}
}
