package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

type checkpointFailureBudgetStore struct {
	store.RunStore
	seen        []context.Context
	fallbackCtx context.Context
}

func (s *checkpointFailureBudgetStore) FailRunResumable(c context.Context, _ string, _ *store.Checkpoint, _ string, _ store.FailureCode) error {
	s.seen = append(s.seen, c)
	<-c.Done()
	return c.Err()
}
func (s *checkpointFailureBudgetStore) UpdateRunStatusCoded(c context.Context, _ string, _ store.RunStatus, _ string, _ store.FailureCode) error {
	s.fallbackCtx = c
	if c.Err() == nil {
		<-c.Done()
	}
	return c.Err()
}
func TestCheckpointFailureBudgetAndInvocation(t *testing.T) {
	for _, door := range []string{"plain", "typed", "ambiguous"} {
		for _, borrow := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/borrow%v", door, borrow), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					base, bound := terminationFixture(t, 1)
					ctx, cancel := context.WithCancel(bound)
					cancel()
					s := &checkpointFailureBudgetStore{RunStore: base}
					e := New(terminationProbeWorkflow(), s, newStubExecutor(), WithTerminationWriteTimeout(2*time.Second))
					rs := e.newRunState("stop", nil)
					rs.ctx = ctx
					var scope *terminationWrites
					if borrow {
						scope = e.newTerminationWrites(ctx, context.Canceled)
						defer scope.Close()
						rs.ctx = scope.ctx
						time.Sleep(time.Second)
					}
					original := error(&RuntimeError{Code: ErrCodeBudgetExceeded, Message: "budget verdict"})
					if door == "ambiguous" {
						original = &RuntimeError{Code: ErrCodeAmbiguousEffect, Message: "unknown effect"}
					}
					start := time.Now()
					var err error
					if door == "plain" {
						err = e.failRunWithCheckpoint(rs, "step", "plain failure")
					} else {
						err = e.failRunErrWithCheckpoint(rs, "step", original)
					}
					elapsed := time.Since(start)
					want := 2 * time.Second
					if borrow {
						want = time.Second
					}
					if elapsed != want || len(s.seen) != 1 || s.fallbackCtx != s.seen[0] {
						t.Fatalf("CHECKPOINT_BUDGET_RENEWED elapsed=%v want=%v calls=%d sameCtx=%v err=%v", elapsed, want, len(s.seen), s.fallbackCtx == s.seen[0], err)
					}
					if borrow && s.seen[0] != scope.ctx {
						t.Fatal("CHECKPOINT_SCOPE_NOT_BORROWED")
					}
					if rs.ctx != ctx && !borrow || borrow && rs.ctx != scope.ctx {
						t.Fatal("RUN_CONTEXT_MUTATED")
					}
					before := len(s.seen)
					got := e.settleLoopPhaseError(rs, "step", err)
					if len(s.seen) != before || got != err {
						t.Fatalf("CHECKPOINT_REENTERED_PHASE calls=%d err=%v", len(s.seen), got)
					}
					if door == "ambiguous" && !IsAmbiguousEffect(err) {
						t.Fatalf("CHECKPOINT_AMBIGUITY_LOST %v", err)
					}
				})
			})
		}
	}
}
func TestCheckpointFailureBorrowDoesNotCloseOrLoseVerdict(t *testing.T) {
	for _, kind := range []string{"plain", "business", "ambiguous"} {
		t.Run(kind, func(t *testing.T) {
			e := New(terminationProbeWorkflow(), nil, newStubExecutor())
			w := e.newTerminationWrites(t.Context(), context.Canceled)
			defer w.Close()
			rs := e.newRunState("stop", nil)
			rs.ctx = w.ctx
			original := errors.New("ordinary cause")
			if kind == "business" {
				original = &RuntimeError{Code: ErrCodeBudgetExceeded, Message: "established business"}
			}
			if kind == "ambiguous" {
				original = &RuntimeError{Code: ErrCodeAmbiguousEffect, Message: "established ambiguous"}
			}
			got := e.withCheckpointFailureWrites(rs, original, func(c context.Context) error {
				if c != w.ctx {
					t.Fatal("BORROWED_CONTEXT_CHANGED")
				}
				return store.ErrValueFormatChanged
			})
			if w.ctx.Err() != nil || w.original != context.Canceled || !errors.Is(got, store.ErrValueFormatChanged) {
				t.Fatalf("BORROWED_SCOPE_MUTATED err=%v ctxerr=%v", got, w.ctx.Err())
			}
			if kind != "plain" && !errors.Is(got, original) {
				t.Fatalf("BORROWED_VERDICT_LOST %v", got)
			}
			calls := 0
			_ = e.withCheckpointFailureWrites(rs, nil, func(context.Context) error { calls++; return nil })
			if calls != 0 {
				t.Fatal("BORROWED_REFUSAL_NOT_STICKY")
			}
		})
	}
}
func TestCheckpointFailureBorrowExpiredSimpleCause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base, bound := terminationFixture(t, 1)
		s := &checkpointFailureBudgetStore{RunStore: base}
		e := New(terminationProbeWorkflow(), s, newStubExecutor(), WithTerminationWriteTimeout(time.Second))
		w := e.newTerminationWrites(bound, context.Canceled)
		defer w.Close()
		rs := e.newRunState("stop", nil)
		rs.ctx = w.ctx
		time.Sleep(time.Second)
		<-w.ctx.Done()
		start := time.Now()
		_ = e.failRunErrWithCheckpoint(rs, "step", w.ctx.Err())
		if time.Since(start) != 0 || len(s.seen) != 1 || s.seen[0] != w.ctx {
			t.Fatalf("EXPIRED_SCOPE_REENTERED_HANDLER elapsed=%v calls=%d", time.Since(start), len(s.seen))
		}
	})
}
func TestCheckpointFailureOwnedOptionAndIsolation(t *testing.T) {
	for _, duration := range []time.Duration{0, 3 * time.Second, -1} {
		t.Run(duration.String(), func(t *testing.T) {
			e := New(terminationProbeWorkflow(), nil, newStubExecutor(), WithTerminationWriteTimeout(duration))
			c, cancel := context.WithCancel(t.Context())
			cancel()
			rs := e.newRunState("stop", nil)
			rs.ctx = c
			var observed context.Context
			got := e.withCheckpointFailureWrites(rs, nil, func(writeCtx context.Context) error {
				observed = writeCtx
				if writeCtx.Err() != nil {
					t.Fatal("CHECKPOINT_CONTEXT_CANCELLED")
				}
				deadline, ok := writeCtx.Deadline()
				if duration < 0 && ok {
					t.Fatal("UNBOUNDED_HAS_DEADLINE")
				}
				if duration >= 0 {
					want := duration
					if want == 0 {
						want = 5 * time.Second
					}
					remaining := time.Until(deadline)
					if !ok || remaining <= 0 || remaining > want {
						t.Fatalf("OPTION_BUDGET_WRONG %v %v", ok, remaining)
					}
				}
				return nil
			})
			if got != nil || duration >= 0 && observed.Err() == nil || rs.ctx != c {
				t.Fatalf("OWNED_LIFETIME_BROKEN err=%v writeErr=%v", got, observed.Err())
			}
		})
	}
	e := New(terminationProbeWorkflow(), nil, newStubExecutor())
	var wg sync.WaitGroup
	for _, refuse := range []bool{true, false} {
		wg.Go(func() {
			rs := e.newRunState("stop", nil)
			rs.ctx = t.Context()
			calls := 0
			err := e.withCheckpointFailureWrites(rs, nil, func(c context.Context) error {
				calls++
				if refuse {
					return store.ErrValueFormatChanged
				}
				return nil
			})
			if calls != 1 || refuse != store.IsValueFormatRefusal(err) {
				t.Errorf("CHECKPOINT_SCOPE_LEAK refuse=%v calls=%d err=%v", refuse, calls, err)
			}
		})
	}
	wg.Wait()
}
func TestCheckpointFailureInitialRefusalPriority(t *testing.T) {
	for _, cause := range []error{store.ErrValueFormatChanged, store.ErrUnsupportedValueFormat, errCorrectionOwnershipLost, errors.Join(errCorrectionOwnershipLost, store.ErrValueFormatChanged)} {
		t.Run(cause.Error(), func(t *testing.T) {
			e := New(terminationProbeWorkflow(), nil, newStubExecutor())
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			rs := e.newRunState("stop", nil)
			rs.ctx = ctx
			if got := e.failRunErrWithCheckpoint(rs, "step", cause); got != cause {
				t.Fatalf("INITIAL_REFUSAL_PRIORITY_CHANGED got=%v want=%v", got, cause)
			}
		})
	}
}
