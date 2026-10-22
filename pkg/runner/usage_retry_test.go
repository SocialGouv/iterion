package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	iterlog "github.com/SocialGouv/iterion/pkg/log"
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

// windowDurationDetail is the INSTANT-LESS form of the window-duration
// refusal shape (pkg/backend/delegate.resetWindowRe): a duration and no date
// at all. The form is the regex's synthetic one — every capture in the tree
// of the facade that prints this sentence renders an absolute instant beside
// the duration (facadeWindowDetail) — and it stays here because the parser
// must answer it too: a provider may print the duration alone, and the
// decision may not depend on which form arrived.
// delegate.ParseResetHint resolves the duration against the moment it is
// read, so every reading of ONE provider window answers a different instant —
// which is what makes the shape one no per-wall bound can see.
const windowDurationDetail = "Usage limit reached for 5 hour. Your limit will reset later."

// windowDurationErr carries that refusal as TEXT only: what a host reports
// when the typed error did not survive the hop
// (delegate.UsageWindowInFlattenedError).
func windowDurationErr() error {
	return &runtime.RuntimeError{
		Code:    runtime.ErrCodeUsageLimitBlocked,
		Message: `node "synthesize" execution failed: rate_limited (claude_code): ` + windowDurationDetail,
	}
}

// facadeWindowDetail is the same refusal the way EVERY capture in the tree
// words it (pkg/backend/delegate/claude_code_ratelimit_test.go:77,
// facade_weekly_wall_test.go:64, usage_limit_test.go:109,
// pkg/runtime/recovery/recovery_test.go:180, CHANGELOG): the duration AND an
// absolute instant the facade resolved when it printed. Whether that instant
// is the duration's own rendering (recomputed per refusal) or the window's
// own anchoring is not visible in one text — which is exactly why
// delegate.ParseResetHintDated answers the question from the pair of facts.
func facadeWindowDetail(at time.Time) string {
	return fmt.Sprintf("Usage limit reached for 5 hour. Your limit will reset at %s",
		at.UTC().Format("2006-01-02 15:04:05"))
}

// facadeWindowErr carries the captured shape as TEXT only, its instant
// rendered at the moment the refusal is read — the facade that recomputes
// the duration against its own clock at every refusal.
func facadeWindowErr(now time.Time) error {
	return &runtime.RuntimeError{
		Code:    runtime.ErrCodeUsageLimitBlocked,
		Message: `node "synthesize" execution failed: rate_limited (claude_code): ` + facadeWindowDetail(now.Add(5*time.Hour)),
	}
}

// facadeWindowTypedErr carries the SAME refusal the way the claude_code
// backend delivers it: the typed error whose ResetAt is the text's own
// absolute instant (classifyRateLimit reads resetAbsRe first), so the typed
// value and the words agree — which is what makes the parser's single ask
// about the words sufficient.
func facadeWindowTypedErr(now time.Time) error {
	detail := facadeWindowDetail(now.Add(5 * time.Hour))
	at, ok := delegate.ParseResetHint(detail, now)
	if !ok {
		panic("fixture: the captured facade shape no longer parses — delegate.ParseResetHint moved")
	}
	return &runtime.RuntimeError{
		Code:    runtime.ErrCodeUsageLimitBlocked,
		Message: `node "synthesize" execution failed: rate_limited (claude_code): ` + detail,
		Cause: &delegate.ErrRateLimited{
			Provider: delegate.BackendClaudeCode,
			Detail:   detail,
			Kind:     delegate.RateLimitKindUsageWindow,
			ResetAt:  at,
		},
	}
}

// facadeWindowErrAt is the captured shape with the instant the text renders
// INDEPENDENT of the reading — the window-anchored facade, whose refusal
// mid-window names the wall with the time that REMAINS of it.
func facadeWindowErrAt(now, resetsAt time.Time) error {
	return &runtime.RuntimeError{
		Code:    runtime.ErrCodeUsageLimitBlocked,
		Message: `node "synthesize" execution failed: rate_limited (claude_code): ` + facadeWindowDetail(resetsAt),
	}
}

// weeklyWallCaptureErr is the multi-day wall's capture
// (pkg/backend/delegate/facade_weekly_wall_test.go:16): an absolute instant
// and NO window duration in the sentence — the provider's own date, with
// nothing in the text that recomputes it.
func weeklyWallCaptureErr(now time.Time) error {
	return &runtime.RuntimeError{
		Code: runtime.ErrCodeUsageLimitBlocked,
		Message: `node "synthesize" execution failed: rate_limited (claude_code): [1310][Weekly/Monthly Limit Exhausted. Your limit will reset at ` +
			now.Add(48*time.Hour).UTC().Format("2006-01-02 15:04:05") + `][20260922194425aaaa]`,
	}
}

// typedWindowDurationErr carries the SAME refusal the way the claude_code
// backend actually delivers it: classifyRateLimit resolves the duration
// against the moment it reads the text and stores THAT instant in the typed
// error's ResetAt (pkg/backend/delegate/claude_code_stream.go,
// claude_code.go), and the engine wraps the typed cause rather than flattening
// it (pkg/runtime.RuntimeError.Cause).
//
// The frozen field is the trap this fixture exists for: it does not move when
// the same provider window is read again, so a reader that asks only the typed
// value cannot tell this shape from a date the provider named.
// delegate.ParseResetHint is called here for the same reason production calls
// it — a fixture that spelled out the instant would stop being the producer's
// shape the day the parser changed.
func typedWindowDurationErr(now time.Time) error {
	at, ok := delegate.ParseResetHint(windowDurationDetail, now)
	if !ok {
		panic("fixture: the window-duration shape no longer parses — delegate.ParseResetHint moved")
	}
	return &runtime.RuntimeError{
		Code:    runtime.ErrCodeUsageLimitBlocked,
		Message: `node "synthesize" execution failed: rate_limited (claude_code): ` + windowDurationDetail,
		Cause: &delegate.ErrRateLimited{
			Provider: delegate.BackendClaudeCode,
			Detail:   windowDurationDetail,
			Kind:     delegate.RateLimitKindUsageWindow,
			ResetAt:  at,
		},
	}
}

// selfImposedCapErr is the refusal iterion RAISES rather than receives: the
// operator's own usage cap firing below the provider's wall
// (pkg/runner/usage_cap.go, pkg/backend/model/executor.go). Its instant comes
// from the cap's own window rather than from any parser, and
// pkg/usagecap.Decision renders it into the reason text — which is what keeps
// it DATED for a reader that asks the refusal's words.
func selfImposedCapErr(resetsAt time.Time) error {
	return &delegate.ErrRateLimited{
		Provider:    delegate.BackendClaudeCode,
		Detail:      fmt.Sprintf("usage cap: 7d window at 100%% ≥ 90%% (forfait, block), resets %s (not started)", resetsAt.UTC().Format(time.RFC3339)),
		Kind:        delegate.RateLimitKindUsageWindow,
		ResetAt:     resetsAt,
		SelfImposed: true,
	}
}

// blindUsageWindowErr is a real usage window whose end nothing parses — the
// case where there is no authoritative instant at all.
func blindUsageWindowErr() error {
	return &runtime.RuntimeError{
		Code:    runtime.ErrCodeUsageLimitBlocked,
		Message: "quota exhausted, no further detail",
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
			// The wake carries the come-back pad (typed and flattened parks of
			// one window wake at the same instant); the wall keeps the raw one.
			name: "code only, instant recovered from the message",
			err: &runtime.RuntimeError{
				Code:    runtime.ErrCodeUsageLimitBlocked,
				Message: `node "synthesize" execution failed: rate_limited (claude_code): You've hit your weekly limit · resets Jul 28, 9pm (UTC)`,
			},
			pol:        noJitter(retrypolicy.Policy{}),
			wantOK:     true,
			wantAt:     reset.Add(time.Minute),
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
			at, source, ok := usageWindowRetryAt(tt.err, tt.pol, retryNow, time.Time{}, 0)
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
		at, _, ok := usageWindowRetryAt(weeklyWindowErr(reset), pol, retryNow, time.Time{}, 0)
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
		at, _, ok := usageWindowRetryAt(weeklyWindowErr(retryNow.Add(90*time.Minute)), pol, retryNow, time.Time{}, 0)
		if !ok {
			t.Fatal("ok = false")
		}
		if at.After(ceiling) {
			t.Fatalf("at = %v exceeds max_wait ceiling %v", at, ceiling)
		}
	}
}

func TestUsageWindowCircuitAt_PreservesJitterAndMaxWait(t *testing.T) {
	openUntil := retryNow.Add(90 * time.Minute)
	state := &store.RetryCircuitState{OpenUntil: &openUntil}

	t.Run("an open circuit is jittered", func(t *testing.T) {
		pol := retrypolicy.Normalize(retrypolicy.Policy{MaxWait: "3h", Jitter: "10m"})
		seen := map[time.Time]bool{}
		for i := 0; i < 200; i++ {
			at, delayed := usageWindowCircuitAt(retryNow.Add(time.Hour), state, pol, retryNow)
			if !delayed {
				t.Fatal("delayed = false, want an open circuit to delay the retry")
			}
			if at.Before(openUntil) || !at.Before(openUntil.Add(10*time.Minute)) {
				t.Fatalf("at = %v, want within [%v, %v)", at, openUntil, openUntil.Add(10*time.Minute))
			}
			seen[at] = true
		}
		if len(seen) < 2 {
			t.Error("circuit jitter produced one instant — the shared circuit would release a synchronized wave")
		}
	})

	t.Run("the resolved max wait remains the ceiling", func(t *testing.T) {
		pol := noJitter(retrypolicy.Policy{MaxWait: "30m"})
		at, delayed := usageWindowCircuitAt(retryNow.Add(10*time.Minute), state, pol, retryNow)
		if !delayed {
			t.Fatal("delayed = false, want the circuit to participate even when its cooldown is clamped")
		}
		if want := retryNow.Add(30 * time.Minute); !at.Equal(want) {
			t.Fatalf("at = %v, want max_wait ceiling %v", at, want)
		}
	})

	t.Run("a circuit that does not extend the retry is ignored", func(t *testing.T) {
		at := retryNow.Add(2 * time.Hour)
		got, delayed := usageWindowCircuitAt(at, state, noJitter(retrypolicy.Policy{}), retryNow)
		if delayed || !got.Equal(at) {
			t.Fatalf("got (%v, %v), want unchanged retry %v", got, delayed, at)
		}
	})
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

// --- the cancelled-context regression ---------------------------------
//
// processOne must stop the heartbeat before finalizing the delivery, and the
// cancel that stops it is the run context's. So by the time the carve-out
// runs, its ctx is ALREADY CANCELLED. The first version passed that ctx
// straight to the store: every call failed instantly, the code logged a
// warning and fell through, and no retry was ever armed in production —
// while the pure-decision and sweeper tests stayed green. Found by review,
// not by tests, which is exactly why this one exists.

// cancelAwareStore fails every call when its context is cancelled, the way
// a real Mongo client does.
type cancelAwareStore struct {
	store.RunStore
	run          *store.Run
	armed        bool
	armedAt      time.Time
	armedWall    time.Time
	calls        int
	blockCircuit bool
	successes    int

	// scheduleErr models the store refusing the PAID arming with an error:
	// the arming lands nothing, the caller naks, and JetStream redelivers
	// against a row no write has touched.
	scheduleErr error

	probeRearmed     bool
	probeRearmedAt   time.Time
	probeRearmedWall time.Time
	probeRearmErr    error
	// probeRearmErrOnce models a TRANSIENT store blip: the first re-arm
	// fails, the next one lands. A fake that fails forever cannot tell a
	// path that gives up from one that tries again.
	probeRearmErrOnce bool
	probeRearmCalls   int
	// probeRearmRefuse models the marker going away under us (the sweeper
	// stood the probe down, or an operator resumed the run): the store
	// answers (false, nil), which is NOT an error and NOT a free re-arm.
	probeRearmRefuse bool

	// abandoned keeps the settlement reasons: a fake that swallows them
	// cannot witness a run retired days before the instant it waits on.
	abandoned       []string
	abandonRefused  int
	circuitFailures int

	events []store.Event
}

func (c *cancelAwareStore) LoadRun(ctx context.Context, _ string) (*store.Run, error) {
	c.calls++
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return c.run, nil
}

func (c *cancelAwareStore) ScheduleRunRetry(ctx context.Context, _ string, at, wall time.Time, _, _ string, maxAttempts int) (bool, int, error) {
	if err := ctx.Err(); err != nil {
		return false, 0, err
	}
	if c.scheduleErr != nil {
		return false, 0, c.scheduleErr
	}
	// The attempt budget is in the real store's FILTER, so a fake that arms
	// unconditionally is kinder than production and hides every
	// budget-exhausted path.
	if spent := c.spentAttempts(); maxAttempts > 0 && spent >= maxAttempts {
		return false, 0, nil
	}
	c.armed, c.armedAt, c.armedWall = true, at, wall
	// The marker is consumed by every arming; the probe BUDGET follows the
	// wall and the PACE stays on the row, exactly as the real store's $cond
	// does — a fake that always cleared the budget would hide the ceiling
	// this change bounds, one that never did would hide the notice the next
	// wall owes, and one that cleared the pace would hide the unbounded
	// pulling a moved wall used to earn.
	if c.run != nil && c.run.RetryState != nil {
		moved := !wall.IsZero() && c.run.RetryState.Wall != nil && !c.run.RetryState.Wall.Equal(wall)
		c.run.RetryState.ProbeFrom = nil
		if !wall.IsZero() {
			w := wall
			c.run.RetryState.Wall = &w
		}
		if moved {
			c.run.RetryState.ProbeCount = 0
		}
	}
	return true, c.spentAttempts() + 1, nil
}

func (c *cancelAwareStore) spentAttempts() int {
	if c.run == nil || c.run.RetryState == nil {
		return 0
	}
	return c.run.RetryState.Attempts
}

func (c *cancelAwareStore) PullRunRetry(ctx context.Context, _ string, _, _ time.Time) (bool, error) {
	return false, ctx.Err()
}

// RearmProbedRunRetry models the store's CAS on the probe marker: no marker,
// no free re-arm.
func (c *cancelAwareStore) RearmProbedRunRetry(ctx context.Context, _ string, at, wall time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	c.probeRearmCalls++
	if c.probeRearmErr != nil && (!c.probeRearmErrOnce || c.probeRearmCalls == 1) {
		return false, c.probeRearmErr
	}
	if c.probeRearmRefuse {
		return false, nil
	}
	if c.run == nil || c.run.RetryState == nil || c.run.RetryState.ProbeFrom == nil {
		return false, nil
	}
	c.run.RetryState.ProbeFrom = nil
	if !wall.IsZero() {
		w := wall
		moved := c.run.RetryState.Wall != nil && !c.run.RetryState.Wall.Equal(w)
		c.run.RetryState.Wall = &w
		if moved {
			c.run.RetryState.ProbeCount = 0
		}
	}
	c.probeRearmed, c.probeRearmedAt, c.probeRearmedWall = true, at, wall
	return true, nil
}

func (c *cancelAwareStore) DelayRunRetry(ctx context.Context, _ string, _, _ time.Time) (bool, error) {
	return false, ctx.Err()
}

func (c *cancelAwareStore) ClaimRunRetry(ctx context.Context, _ string, _ time.Time) (bool, error) {
	return false, ctx.Err()
}

func (c *cancelAwareStore) ClearRunRetry(ctx context.Context, _ string) error { return ctx.Err() }

func (c *cancelAwareStore) AbandonRunRetry(ctx context.Context, _, reason string, expectedAfter time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	// The real store conditions on the armed instant the caller read: a row a
	// sweep tick moved in between is not this caller's to retire.
	if !expectedAfter.IsZero() {
		st := c.run.RetryState
		if st == nil || st.RetryAfter == nil || !st.RetryAfter.Equal(expectedAfter) {
			c.abandonRefused++
			return false, nil
		}
	}
	c.abandoned = append(c.abandoned, reason)
	// Model the real store: the abandon $unsets both the armed instant and
	// the probe marker, which is what makes settling a probed row
	// irreversible.
	if c.run != nil && c.run.RetryState != nil {
		c.run.RetryState.RetryAfter, c.run.RetryState.ProbeFrom = nil, nil
	}
	return true, nil
}

func (c *cancelAwareStore) RecordRetryFailure(context.Context, string, string, time.Time, int, time.Duration) (*store.RetryCircuitState, error) {
	c.circuitFailures++
	return nil, nil
}

// RetryCircuitOpen models the read that moves the wake behind an already
// open breaker. blockCircuit models a HUNG circuit store here rather than on
// the record: the record now runs after the arming has landed, so a slow
// circuit store can only delay the arming through the read.
func (c *cancelAwareStore) RetryCircuitOpen(ctx context.Context, _ string, _ time.Time) (*store.RetryCircuitState, error) {
	if c.blockCircuit {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return nil, nil
}

func (c *cancelAwareStore) RecordRetrySuccess(context.Context, string, time.Time) error {
	c.successes++
	return nil
}

func TestRetryCircuitResetRequiresDurableFinishedStatus(t *testing.T) {
	st := &cancelAwareStore{run: &store.Run{
		ID: "run-review-reply", WorkflowHash: "rev-1", Status: store.RunStatusPausedWaitingHuman,
	}}
	r := &Runner{cfg: Config{Store: st}}
	r.resetRetryCircuitAfterSuccessfulExecution(context.Background(), st.run.ID)
	if st.successes != 0 {
		t.Fatalf("paused review reply reset retry circuit %d times, want 0", st.successes)
	}

	st.run.Status = store.RunStatusFinished
	r.resetRetryCircuitAfterSuccessfulExecution(context.Background(), st.run.ID)
	if st.successes != 1 {
		t.Fatalf("finished run reset retry circuit %d times, want 1", st.successes)
	}
}

func (c *cancelAwareStore) AppendEvent(ctx context.Context, _ string, ev store.Event) (*store.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Kept, not swallowed: the timeline event is what an operator reads to
	// tell one arming from another.
	c.events = append(c.events, ev)
	return &store.Event{}, nil
}

func TestArmUsageWindowRetry_SurvivesACancelledContext(t *testing.T) {
	st := &cancelAwareStore{run: &store.Run{ID: "run-a", Status: store.RunStatusFailedResumable}}
	r := &Runner{cfg: Config{Store: st}}

	// Exactly what the call site hands over: a context already cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got := r.armUsageWindowRetry(ctx, weeklyWindowErr(time.Now().UTC().Add(30*time.Hour)), "run-a", spentNothing, iterlog.New(iterlog.LevelError, io.Discard))
	if got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed — a cancelled ctx must not disable the carve-out", got)
	}
	if !st.armed {
		t.Fatal("no retry persisted: the store calls ran on the cancelled context")
	}
	if st.armedAt.IsZero() {
		t.Error("retry armed with a zero instant")
	}
}

func TestArmUsageWindowRetry_SlowCircuitLeavesTimeForMandatoryArm(t *testing.T) {
	st := &cancelAwareStore{
		run:          &store.Run{ID: "run-slow-circuit", Status: store.RunStatusFailedResumable, WorkflowHash: "rev-1"},
		blockCircuit: true,
	}
	r := &Runner{cfg: Config{Store: st}}

	started := time.Now()
	got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(time.Now().UTC().Add(time.Hour)), "run-slow-circuit", spentNothing, iterlog.New(iterlog.LevelError, io.Discard))
	if got != usageRetryArmed || !st.armed {
		t.Fatalf("outcome = %v armed=%v, want mandatory per-run retry after circuit timeout", got, st.armed)
	}
	if elapsed := time.Since(started); elapsed >= usageRetryStoreTimeout {
		t.Fatalf("circuit consumed the full %v store budget (elapsed %v)", usageRetryStoreTimeout, elapsed)
	}
}

// TestArmUsageWindowRetry_NotAUsageWindow pins that an unrelated failure
// still falls through to the caller's nak/DLQ path.
func TestArmUsageWindowRetry_NotAUsageWindow(t *testing.T) {
	st := &cancelAwareStore{run: &store.Run{ID: "run-b", Status: store.RunStatusFailedResumable}}
	r := &Runner{cfg: Config{Store: st}}

	got := r.armUsageWindowRetry(context.Background(), errors.New("tool blew up"), "run-b", spentNothing, iterlog.New(iterlog.LevelError, io.Discard))
	if got != usageRetryNotApplicable {
		t.Errorf("outcome = %v, want usageRetryNotApplicable", got)
	}
	if st.armed {
		t.Error("armed a retry for a failure that is not a usage window")
	}
}

// TestArmUsageWindowRetry_NoRetryStoreFallsThrough pins the local-mode path:
// a store with no durable retry surface must not swallow the delivery.
func TestArmUsageWindowRetry_NoRetryStoreFallsThrough(t *testing.T) {
	r := &Runner{cfg: Config{Store: plainStore{}}}
	got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(time.Now().UTC().Add(time.Hour)), "run-c", spentNothing, iterlog.New(iterlog.LevelError, io.Discard))
	if got != usageRetryNotApplicable {
		t.Errorf("outcome = %v, want usageRetryNotApplicable", got)
	}
}

// plainStore satisfies store.RunStore without store.RunRetryStore.
type plainStore struct{ store.RunStore }
