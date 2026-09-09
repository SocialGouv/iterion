package runner

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/retrypolicy"
	"github.com/SocialGouv/iterion/pkg/store"
)

// circuitStore is a cancelAwareStore that ALSO carries the optional
// RetryCircuitStore capability, so armUsageWindowRetry takes the coordinated
// branch with no Mongo server anywhere. The capability is an interface
// assertion on the store value, which is exactly what this fake reproduces.
type circuitStore struct {
	*cancelAwareStore
	openUntil *time.Time
	recordErr error
	// hang makes RecordRetryFailure wait for its context instead of
	// returning, the way a wedged Mongo primary does.
	hang bool

	failures  int
	successes int
	lastKey   string
	lastEvent store.Event
}

func (c *circuitStore) RecordRetryFailure(ctx context.Context, key, _ string, _ time.Time, _ int, _ time.Duration) (*store.RetryCircuitState, error) {
	c.failures++
	c.lastKey = key
	if c.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if c.recordErr != nil {
		return nil, c.recordErr
	}
	return &store.RetryCircuitState{Key: key, ConsecutiveFailures: c.failures, OpenUntil: c.openUntil}, nil
}

func (c *circuitStore) RetryCircuitOpen(_ context.Context, key string, _ time.Time) (*store.RetryCircuitState, error) {
	if c.openUntil == nil {
		return nil, nil
	}
	return &store.RetryCircuitState{Key: key, OpenUntil: c.openUntil}, nil
}

func (c *circuitStore) RecordRetrySuccess(context.Context, string, time.Time) error {
	c.successes++
	return nil
}

// AppendEvent captures run_retry_scheduled so the reset_source the operator
// reads is assertable — a source claiming "+circuit_open" for a wait the
// breaker did not actually contribute is itself the bug.
func (c *circuitStore) AppendEvent(ctx context.Context, runID string, e store.Event) (*store.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.lastEvent = e
	return &e, nil
}

func newCircuitStore(t *testing.T, pol *store.RunRetryPolicy, openUntil *time.Time) *circuitStore {
	t.Helper()
	return &circuitStore{
		cancelAwareStore: &cancelAwareStore{run: &store.Run{
			ID:           "run-circuit",
			Status:       store.RunStatusFailedResumable,
			WorkflowHash: "wfhash",
			RetryPolicy:  pol,
		}},
		openUntil: openUntil,
	}
}

func (c *circuitStore) resetSource() string {
	s, _ := c.lastEvent.Data["reset_source"].(string)
	return s
}

// TestArmUsageWindowRetry_CircuitCannotOutlastMaxWait is the regression test
// for the gate-blocking defect: the breaker's cooldown is a deployment-wide
// env value, and an authority outside the run may only ever LOWER the run's
// own policy. A cooldown reaching past max_wait must be clamped back to the
// horizon, not adopted raw.
func TestArmUsageWindowRetry_CircuitCannotOutlastMaxWait(t *testing.T) {
	openUntil := time.Now().UTC().Add(72 * time.Hour)
	st := newCircuitStore(t, &store.RunRetryPolicy{MaxWait: "2h", Jitter: "0s"}, &openUntil)
	r := &Runner{cfg: Config{Store: st}}

	start := time.Now().UTC()
	got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(start.Add(30*time.Hour)), "run-circuit", iterlog.New(iterlog.LevelError, io.Discard))
	if got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed", got)
	}
	if st.failures != 1 {
		t.Fatalf("circuit failures = %d, want 1 — the coordinated branch never ran", st.failures)
	}
	ceiling := start.Add(2 * time.Hour)
	if st.armedAt.After(ceiling.Add(2 * time.Second)) {
		t.Errorf("armed at %s, past the policy horizon %s — the circuit cooldown escaped max_wait",
			st.armedAt.Format(time.RFC3339), ceiling.Format(time.RFC3339))
	}
	// The cooldown bought nothing once clamped, so the source must not claim
	// the breaker moved the wake-up.
	if src := st.resetSource(); src != "typed_error" {
		t.Errorf("reset_source = %q, want %q — a clamped cooldown did not contribute", src, "typed_error")
	}
}

// TestArmUsageWindowRetry_CircuitMovesWakeUpWithinHorizon pins the other
// half: a cooldown the policy CAN accommodate is adopted, and says so.
func TestArmUsageWindowRetry_CircuitMovesWakeUpWithinHorizon(t *testing.T) {
	start := time.Now().UTC()
	openUntil := start.Add(3 * time.Hour)
	st := newCircuitStore(t, &store.RunRetryPolicy{Jitter: "0s"}, &openUntil)
	r := &Runner{cfg: Config{Store: st}}

	// Reset one hour out: without the breaker the retry would arm at ~+1h01.
	got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(start.Add(time.Hour)), "run-circuit", iterlog.New(iterlog.LevelError, io.Discard))
	if got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed", got)
	}
	if st.armedAt.Before(openUntil.Add(-2 * time.Second)) {
		t.Errorf("armed at %s, before the breaker cooldown %s", st.armedAt.Format(time.RFC3339), openUntil.Format(time.RFC3339))
	}
	if src := st.resetSource(); src != "typed_error+circuit_open" {
		t.Errorf("reset_source = %q, want typed_error+circuit_open", src)
	}
}

// TestArmUsageWindowRetry_CircuitOutageStillArms pins the documented
// degradation: a circuit-store outage must not cost the run its durable
// per-run retry.
func TestArmUsageWindowRetry_CircuitOutageStillArms(t *testing.T) {
	st := newCircuitStore(t, nil, nil)
	st.recordErr = errors.New("mongo down")
	r := &Runner{cfg: Config{Store: st}}

	got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(time.Now().UTC().Add(30*time.Hour)), "run-circuit", iterlog.New(iterlog.LevelError, io.Discard))
	if got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed — a circuit outage must not drop the retry", got)
	}
	if !st.armed {
		t.Error("no retry persisted after a circuit-store outage")
	}
}

// TestArmUsageWindowRetry_SlowCircuitStillArms is the other half of that
// degradation, and the one the error path does not cover: a store outage
// usually presents as LATENCY, not as a prompt error. The circuit update
// runs before ScheduleRunRetry and used to share the arming's single 10s
// deadline, so a wedged circuit collection drained the whole budget and left
// the essential write an expired context — falling the run back to
// redelivery, which is the pod storm the carve-out exists to prevent, caused
// by the breaker meant to damp it.
func TestArmUsageWindowRetry_SlowCircuitStillArms(t *testing.T) {
	st := newCircuitStore(t, nil, nil)
	st.hang = true
	r := &Runner{cfg: Config{Store: st}}

	start := time.Now()
	got := r.armUsageWindowRetry(context.Background(), weeklyWindowErr(time.Now().UTC().Add(30*time.Hour)), "run-circuit", iterlog.New(iterlog.LevelError, io.Discard))
	elapsed := time.Since(start)

	if got != usageRetryArmed {
		t.Fatalf("outcome = %v, want usageRetryArmed — a hung circuit store cost the run its durable retry", got)
	}
	if !st.armed {
		t.Error("no retry persisted: the circuit update consumed the arming's store budget")
	}
	// It must give up on its own slice, well before the arming's own bound.
	if elapsed >= usageRetryStoreTimeout {
		t.Errorf("arming took %s, at or past the whole store budget (%s) — the circuit call is not separately bounded",
			elapsed, usageRetryStoreTimeout)
	}
}

// TestCircuitCooldownAt_ClampsToPolicyHorizon exercises the arithmetic
// directly, without the store round-trip.
func TestCircuitCooldownAt_ClampsToPolicyHorizon(t *testing.T) {
	now := retryNow
	pol := noJitter(retrypolicy.Policy{MaxWait: "2h"})
	at := now.Add(2 * time.Hour) // already clamped by usageWindowRetryAt

	got, ok := circuitCooldownAt(now.Add(72*time.Hour), at, pol, now)
	if ok {
		t.Error("ok = true, but the ceiling clamped the cooldown back to at — the breaker contributed nothing")
	}
	if !got.Equal(at) {
		t.Errorf("at = %s, want %s (unchanged)", got.Format(time.RFC3339), at.Format(time.RFC3339))
	}
}

// TestCircuitCooldownAt_NeverPullsTheRetryEarlier: the breaker may only ever
// delay a wake-up. A clamp that landed before the evidence's own instant
// would resume the run INTO the wall it is waiting on.
func TestCircuitCooldownAt_NeverPullsTheRetryEarlier(t *testing.T) {
	now := retryNow
	pol := noJitter(retrypolicy.Policy{MaxWait: "1h"})
	at := now.Add(90 * time.Minute) // deliberately past the horizon

	got, ok := circuitCooldownAt(now.Add(48*time.Hour), at, pol, now)
	if ok {
		t.Error("ok = true, but the clamped cooldown lands before at")
	}
	if got.Before(at) {
		t.Errorf("at = %s, earlier than the instant the evidence chose (%s)", got.Format(time.RFC3339), at.Format(time.RFC3339))
	}
}

// TestCircuitCooldownAt_SpreadsRunsSharingOneBreaker is the anti-herd
// guarantee. OpenUntil is ONE durable instant every run behind the breaker
// reads, so adopting it raw arms them all for the identical moment — the
// synchronized wave (five feed-watch digests) the jitter exists to prevent.
func TestCircuitCooldownAt_SpreadsRunsSharingOneBreaker(t *testing.T) {
	now := retryNow
	pol := retrypolicy.Normalize(retrypolicy.Policy{Jitter: "10m"})
	openUntil := now.Add(15 * time.Minute)
	at := now.Add(6 * time.Minute)

	seen := map[time.Time]int{}
	for range 64 {
		got, ok := circuitCooldownAt(openUntil, at, pol, now)
		if !ok {
			t.Fatalf("cooldown %s not adopted over at %s", openUntil.Format(time.RFC3339), at.Format(time.RFC3339))
		}
		if got.Before(openUntil) || got.After(openUntil.Add(10*time.Minute)) {
			t.Fatalf("spread instant %s outside [openUntil, openUntil+jitter]", got.Format(time.RFC3339Nano))
		}
		seen[got]++
	}
	if len(seen) < 2 {
		t.Error("every run behind the breaker got the identical instant — the anti-herd spread was dropped")
	}
}

// TestCircuitCooldownAt_ZeroJitterAdoptsExactly keeps an explicit "0s" spread
// meaningful: an operator who disabled the jitter gets the cooldown itself.
func TestCircuitCooldownAt_ZeroJitterAdoptsExactly(t *testing.T) {
	now := retryNow
	pol := noJitter(retrypolicy.Policy{})
	openUntil := now.Add(30 * time.Minute)

	got, ok := circuitCooldownAt(openUntil, now.Add(6*time.Minute), pol, now)
	if !ok || !got.Equal(openUntil) {
		t.Errorf("at = %s (ok=%v), want %s", got.Format(time.RFC3339), ok, openUntil.Format(time.RFC3339))
	}
}
