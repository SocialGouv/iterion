package runtime

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

type fanoutWriteObservation struct {
	Method  string
	Context error
	Result  error
	Status  store.RunStatus
	Code    store.FailureCode
}
type fanoutObservedStore struct {
	store.RunStore
	mu     sync.Mutex
	writes []fanoutWriteObservation
}

func (s *fanoutObservedStore) Unwrap() store.RunStore { return s.RunStore }
func (s *fanoutObservedStore) note(method string, ctx context.Context, err error, status store.RunStatus, code store.FailureCode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writes = append(s.writes, fanoutWriteObservation{method, ctx.Err(), err, status, code})
}
func (s *fanoutObservedStore) FailRunResumable(c context.Context, id string, cp *store.Checkpoint, message string, code store.FailureCode) error {
	err := s.RunStore.FailRunResumable(c, id, cp, message, code)
	s.note("resumable", c, err, store.RunStatusFailedResumable, code)
	return err
}
func (s *fanoutObservedStore) UpdateRunStatusCoded(c context.Context, id string, status store.RunStatus, message string, code store.FailureCode) error {
	err := s.RunStore.UpdateRunStatusCoded(c, id, status, message, code)
	s.note("fallback-status", c, err, status, code)
	return err
}
func (s *fanoutObservedStore) AppendEvent(c context.Context, id string, e store.Event) (*store.Event, error) {
	got, err := s.RunStore.AppendEvent(c, id, e)
	if e.Type == store.EventRunFailed || e.Type == store.EventRunCancelled {
		s.note("terminal-event", c, err, "", "")
	}
	return got, err
}
func TestFanoutTerminalWriteObservation(t *testing.T) {
	for _, kind := range []string{"healthy", "cancel", "interrupt", "deadline"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				wf := fanOutWorkflow(ir.AwaitWaitAll)
				base := tmpStore(t)
				s := &fanoutObservedStore{RunStore: base}
				var ctx context.Context
				var cancel func()
				if kind == "deadline" {
					ctx, cancel = context.WithTimeout(t.Context(), time.Hour)
				} else {
					c, stop := context.WithCancelCause(t.Context())
					ctx = c
					cancel = func() {
						if kind == "interrupt" {
							stop(ErrRunInterrupted)
						} else {
							stop(context.Canceled)
						}
					}
				}
				defer cancel()
				var entered sync.WaitGroup
				entered.Add(2)
				var completed sync.WaitGroup
				completed.Add(1)
				go func() {
					defer completed.Done()
					entered.Wait()
					if kind == "deadline" {
						time.Sleep(time.Hour)
					} else if kind != "healthy" {
						cancel()
					}
				}()
				x := newStubExecutor()
				x.on("entry", func(map[string]any) (map[string]any, error) { return map[string]any{"summary": "go"}, nil })
				for _, name := range []string{"agent_a", "agent_b"} {
					x.on(name, func(map[string]any) (map[string]any, error) {
						entered.Done()
						if kind != "healthy" {
							<-ctx.Done()
							return nil, ctx.Err()
						}
						return map[string]any{}, nil
					})
				}
				err := New(wf, s, x).Run(ctx, "fanout-observe", nil)
				completed.Wait()
				r, re := base.LoadRun(t.Context(), "fanout-observe")
				if re != nil {
					t.Fatal(re)
				}
				s.mu.Lock()
				rows := append([]fanoutWriteObservation(nil), s.writes...)
				s.mu.Unlock()
				t.Logf("FANOUT_OBSERVED kind=%s status=%s code=%s err=%v cancelled=%v interrupted=%v ctxcause=%v writes=%+v", kind, r.Status, r.FailureCode, err, errors.Is(err, ErrRunCancelled), errors.Is(err, ErrRunInterrupted), context.Cause(ctx), rows)
				if kind == "healthy" && (err != nil || r.Status != store.RunStatusFinished) {
					t.Fatalf("HEALTHY_CHANGED: %v %+v", err, r)
				}
				if kind != "healthy" && err == nil {
					t.Fatal("MISSED_CANCELLATION")
				}
				if kind == "cancel" {
					if len(rows) == 0 {
						t.Fatal("MISSED_FAILURE_WRITER")
					}
					if r.Status != store.RunStatusCancelled {
						t.Errorf("FANOUT_TERMINATION_UNPERSISTED status=%s err=%v", r.Status, err)
					}
				}
			})
		})
	}
}
func TestFanoutCancellationWrapperObservation(t *testing.T) {
	e := &Engine{}
	err := e.wrapContextErr(context.Canceled)
	t.Logf("WRAPPER raw=%v cancelled=%v context=%v", err, errors.Is(err, ErrRunCancelled), errors.Is(err, context.Canceled))
	if !errors.Is(err, ErrRunCancelled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("unexpected wrapper: %v", err)
	}
}
