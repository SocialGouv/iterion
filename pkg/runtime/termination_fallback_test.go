package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

type terminationCallbackFallbackStore struct {
	*terminationPublicStore
	t         *testing.T
	bound     context.Context
	format    store.ValueFormat
	swap      bool
	successor *store.Run
	eventErr  error
}

func (s *terminationCallbackFallbackStore) FailRunResumable(c context.Context, id string, cp *store.Checkpoint, msg string, code store.FailureCode) error {
	s.seen["resumable"]++
	return errors.New("first ordinary disk failure")
}
func (s *terminationCallbackFallbackStore) AppendEvent(c context.Context, id string, e store.Event) (*store.Event, error) {
	if e.Type == store.EventRunFailed {
		s.seen["event"]++
		if s.swap {
			s.successor = terminationCallbackReplacement(s.t, s.RunStore, s.bound, 3-s.format)
		}
	}
	got, err := s.RunStore.AppendEvent(c, id, e)
	if e.Type == store.EventRunFailed {
		s.eventErr = err
	}
	return got, err
}
func TestTerminationFallbackEventRefusal(t *testing.T) {
	for _, format := range []store.ValueFormat{1, 2} {
		for _, kind := range []string{"interruption", "deadline"} {
			for _, swap := range []bool{false, true} {
				t.Run(fmt.Sprintf("format%d/%s/swap%v", format, kind, swap), func(t *testing.T) {
					synctest.Test(t, func(t *testing.T) {
						base, bound := terminationFixture(t, format)
						var ctx context.Context
						var cancel func()
						if kind == "deadline" {
							ctx, cancel = context.WithTimeout(bound, time.Hour)
						} else {
							c, cc := context.WithCancelCause(bound)
							ctx = c
							cancel = func() { cc(ErrRunInterrupted) }
						}
						defer cancel()
						s := &terminationCallbackFallbackStore{terminationPublicStore: &terminationPublicStore{RunStore: base, seen: map[string]int{}}, t: t, bound: bound, format: format, swap: swap}
						x := newStubExecutor()
						x.on("step", func(map[string]any) (map[string]any, error) {
							if kind == "deadline" {
								time.Sleep(time.Hour)
								<-ctx.Done()
							} else {
								cancel()
							}
							return nil, ctx.Err()
						})
						err := New(terminationProbeWorkflow(), s, x).Run(ctx, "stop", nil)
						got, re := base.LoadRun(t.Context(), "stop")
						if re != nil {
							t.Fatal(re)
						}
						t.Logf("err=%v formatRefusal=%v status=%s callbacks=%v", err, store.IsValueFormatRefusal(err), got.Status, s.seen)
						if s.seen["resumable"] != 1 || s.seen["fallback"] != 1 || s.seen["event"] != 1 {
							t.Fatalf("missed fixture: %v", s.seen)
						}
						if swap {
							if !errors.Is(s.eventErr, store.ErrValueFormatChanged) {
								t.Fatalf("REAL_APPEND_DID_NOT_REFUSE: %v", s.eventErr)
							}
							if !errors.Is(err, store.ErrValueFormatChanged) || !reflect.DeepEqual(got, s.successor) {
								t.Fatalf("FALLBACK_EVENT_REFUSAL_SWALLOWED err=%v equalSuccessor=%v", err, reflect.DeepEqual(got, s.successor))
							}
						} else {
							var rt *RuntimeError
							if !errors.As(err, &rt) || rt.Code != ErrCodeExecutionFailed || got.Status != store.RunStatusFailed {
								t.Fatalf("HEALTHY_FALLBACK_CHANGED err=%v status=%s", err, got.Status)
							}
						}
					})
				})
			}
		}
	}
}
func TestTerminationNestedEstablishedVerdict(t *testing.T) {
	business := &RuntimeError{Code: ErrCodeBudgetExceeded, Message: "established business"}
	for _, original := range []error{business, &RuntimeError{Code: ErrCodeExecutionFailed, Message: "neutral outer", Cause: business}, errors.Join(&RuntimeError{Code: ErrCodeExecutionFailed, Message: "neutral sibling"}, business)} {
		t.Run(original.Error(), func(t *testing.T) {
			w := (&Engine{}).newTerminationWrites(t.Context(), original)
			defer w.Close()
			_ = w.Do(func(context.Context) error { return store.ErrValueFormatChanged })
			got := w.Result(ErrRunCancelled)
			if !errors.Is(got, business) || !errors.Is(got, store.ErrValueFormatChanged) {
				t.Fatalf("ESTABLISHED_BUSINESS_LOST original=%v got=%v", original, got)
			}
		})
	}
}

func terminationCallbackReplacement(t *testing.T, base store.RunStore, bound context.Context, format store.ValueFormat) *store.Run {
	t.Helper()
	if err := base.DeleteRun(bound, "stop"); err != nil {
		t.Fatal(err)
	}
	p := base.(interface {
		PruneDeletionMarkers(context.Context, time.Time) (int, error)
	})
	if n, err := p.PruneDeletionMarkers(store.WithTenant(t.Context(), "original-tenant"), time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil || n == 0 {
		t.Fatalf("fixture prune n=%d err=%v", n, err)
	}
	if _, err := store.CreateRunWithValueFormat(t.Context(), base, store.RunCreateSpec{ID: "stop", WorkflowName: "successor", Format: format}); err != nil {
		t.Fatal(err)
	}
	r, err := base.LoadRun(t.Context(), "stop")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type terminationCallbackBudgetStore struct {
	*terminationPublicStore
	resumableCalls int
}

func (s *terminationCallbackBudgetStore) FailRunResumable(c context.Context, id string, cp *store.Checkpoint, msg string, code store.FailureCode) error {
	s.resumableCalls++
	<-c.Done()
	return c.Err()
}
func TestTerminationDoesNotRestartTerminationBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base, bound := terminationFixture(t, 2)
		ctx, cancel := context.WithTimeout(bound, time.Hour)
		defer cancel()
		s := &terminationCallbackBudgetStore{terminationPublicStore: &terminationPublicStore{RunStore: base, seen: map[string]int{}}}
		x := newStubExecutor()
		var stopped time.Time
		x.on("step", func(map[string]any) (map[string]any, error) {
			time.Sleep(time.Hour)
			<-ctx.Done()
			stopped = time.Now()
			return nil, ctx.Err()
		})
		err := New(terminationProbeWorkflow(), s, x, WithTerminationWriteTimeout(2*time.Second)).Run(ctx, "stop", nil)
		elapsed := time.Since(stopped)
		t.Logf("error=%v resumableCalls=%d elapsed=%v", err, s.resumableCalls, elapsed)
		if s.resumableCalls != 1 || elapsed != 2*time.Second {
			t.Fatalf("TERMINATION_HANDLER_REENTERED calls=%d elapsed=%v err=%v", s.resumableCalls, elapsed, err)
		}
	})
}

// This decorator preserves the real optional accounting capability. It is only
// used by the second phase witness, so the correction call reaches real FS CAS.
type terminationCallbackAccountingBudgetStore struct {
	*terminationCallbackBudgetStore
}

func (s *terminationCallbackAccountingBudgetStore) Unwrap() store.RunStore { return s.RunStore }
func TestTerminationAfterExecDoesNotRestartTerminationBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		base, bound := terminationFixture(t, 2)
		ctx, cancel := context.WithTimeout(bound, 10*time.Second)
		defer cancel()
		s := &terminationCallbackAccountingBudgetStore{&terminationCallbackBudgetStore{terminationPublicStore: &terminationPublicStore{RunStore: base, seen: map[string]int{}}}}
		x := &usageCorrectingExecutor{stubExecutor: newStubExecutor()}
		x.on("my_agent", func(map[string]any) (map[string]any, error) { return map[string]any{"summary": 1}, nil })
		var stopped time.Time
		x.correct = func(c context.Context, _ map[string]any, _ error) (map[string]any, OutputCorrectionUsage, error) {
			time.Sleep(10 * time.Second)
			<-c.Done()
			stopped = time.Now()
			return nil, OutputCorrectionUsage{}, c.Err()
		}
		err := New(validationWorkflow(), s, x, WithOutputValidation(true), WithOutputCorrectionBudget(1), WithTerminationWriteTimeout(2*time.Second)).Run(ctx, "stop", nil)
		elapsed := time.Since(stopped)
		t.Logf("error=%v correctionCalls=%d resumableCalls=%d elapsed=%v", err, x.calls, s.resumableCalls, elapsed)
		if x.calls != 1 {
			t.Fatalf("MISSED_REAL_CORRECTION: %v", err)
		}
		if s.resumableCalls != 1 || elapsed != 2*time.Second {
			t.Fatalf("AFTER_EXEC_HANDLER_REENTERED calls=%d elapsed=%v err=%v", s.resumableCalls, elapsed, err)
		}
	})
}

type terminationCallbackEventStore struct {
	store.RunStore
	t        *testing.T
	bound    context.Context
	swap     bool
	ordinary bool
	eventErr error
	calls    int
}

func (s *terminationCallbackEventStore) AppendEvent(c context.Context, id string, e store.Event) (*store.Event, error) {
	if e.Type != store.EventRunFailed {
		return s.RunStore.AppendEvent(c, id, e)
	}
	s.calls++
	if s.ordinary {
		return nil, errors.New("ordinary event I/O")
	}
	if s.swap {
		terminationCallbackReplacement(s.t, s.RunStore, s.bound, 1)
	}
	got, err := s.RunStore.AppendEvent(c, id, e)
	s.eventErr = err
	return got, err
}
func TestTerminationOpaqueCallbackClass(t *testing.T) {
	for _, name := range []string{"failRun", "failRunWithCode", "failRunErr", "emitRunFailedAndReturn", "failRunWithCheckpoint", "failRunErrWithCheckpoint", "failRunDeliberate"} {
		for _, mode := range []string{"scoped-refusal", "unscoped-refusal", "scoped-ordinary", "scoped-healthy"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				base, bound := terminationFixture(t, 2)
				s := &terminationCallbackEventStore{RunStore: base, t: t, bound: bound, swap: mode == "scoped-refusal" || mode == "unscoped-refusal", ordinary: mode == "scoped-ordinary"}
				e := New(terminationProbeWorkflow(), s, newStubExecutor())
				rs := e.newRunState("stop", nil)
				business := &RuntimeError{Code: ErrCodeBudgetExceeded, Message: "budget"}
				call := func(c context.Context) error {
					rs.ctx = c
					switch name {
					case "failRun":
						return e.failRun(c, "stop", "step", "failure")
					case "failRunWithCode":
						return e.failRunWithCode(c, "stop", "step", "failure", ErrCodeBudgetExceeded, "")
					case "failRunErr":
						return e.failRunErr(c, "stop", "step", business)
					case "emitRunFailedAndReturn":
						return e.emitRunFailedAndReturn(c, "stop", "step", "failure", ErrCodeTimeout)
					case "failRunWithCheckpoint":
						return e.failRunWithCheckpoint(rs, "step", "failure")
					case "failRunErrWithCheckpoint":
						return e.failRunErrWithCheckpoint(rs, "step", business)
					case "failRunDeliberate":
						return e.failRunDeliberate(rs, "step", failNodeOutcome{reason: "failure", code: ErrCodeBudgetExceeded})
					default:
						panic(name)
					}
				}
				var got error
				if mode == "unscoped-refusal" {
					got = call(bound)
				} else {
					w := e.newTerminationWrites(bound, context.Canceled)
					defer w.Close()
					got = w.Result(w.Do(call))
					if mode == "scoped-refusal" {
						after := 0
						_ = w.Do(func(context.Context) error { after++; return nil })
						if after != 0 {
							t.Fatal("OPAQUE_REFUSAL_NOT_STICKY")
						}
					}
				}
				if s.calls != 1 {
					t.Fatalf("fixture events=%d err=%v", s.calls, got)
				}
				if s.swap && !errors.Is(s.eventErr, store.ErrValueFormatChanged) {
					t.Fatalf("REAL_APPEND_NOT_REFUSED: %v", s.eventErr)
				}
				// Checkpoint entries create their own sequence when their caller
				// has none. The other five writers still borrow only an existing one.
				ownsSequence := name == "failRunWithCheckpoint" || name == "failRunErrWithCheckpoint"
				if mode == "scoped-refusal" || mode == "unscoped-refusal" && ownsSequence {
					if !errors.Is(got, store.ErrValueFormatChanged) {
						t.Fatalf("OPAQUE_REFUSAL_LOST: %v", got)
					}
				} else {
					var rt *RuntimeError
					if store.IsValueFormatRefusal(got) || !errors.As(got, &rt) {
						t.Fatalf("ORDINARY_OR_UNSCOPED_POLICY_CHANGED: %v", got)
					}
				}
			})
		}
	}
}

type terminationCallbackRedacted struct{ error }

func (terminationCallbackRedacted) Error() string     { return "redacted" }
func (e terminationCallbackRedacted) As(v any) bool   { return errors.As(e.error, v) }
func (e terminationCallbackRedacted) Is(v error) bool { return errors.Is(e.error, v) }
func TestTerminationEstablishedAsOnlyControl(t *testing.T) {
	business := &RuntimeError{Code: ErrCodeBudgetExceeded, Message: "private business"}
	for _, cause := range []error{business, &RuntimeError{Code: ErrCodeExecutionFailed, Cause: business}, errors.Join(&RuntimeError{Code: ErrCodeExecutionFailed}, business)} {
		original := terminationCallbackRedacted{cause}
		w := (&Engine{}).newTerminationWrites(t.Context(), original)
		defer w.Close()
		_ = w.Do(func(context.Context) error { return store.ErrValueFormatChanged })
		got := w.Result(ErrRunCancelled)
		if !errors.Is(got, business) || !errors.Is(got, store.ErrValueFormatChanged) {
			t.Fatalf("AS_ONLY_VERDICT_LOST: %v", got)
		}
	}
}

// Deterministic timeout double: a required write never wins a ready filesystem
// lock after its context already expired. The actual context supplies the error.
func (s *terminationCallbackBudgetStore) UpdateRunStatusCoded(c context.Context, id string, status store.RunStatus, msg string, code store.FailureCode) error {
	if err := c.Err(); err != nil {
		return err
	}
	return s.terminationPublicStore.UpdateRunStatusCoded(c, id, status, msg, code)
}
