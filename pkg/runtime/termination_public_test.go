package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

type terminationProbeKey struct{}
type terminationPublicStore struct {
	store.RunStore
	at     string
	once   bool
	action func(context.Context) error
	seen   map[string]int
	check  func(context.Context)
}

func (s *terminationPublicStore) hit(c context.Context, stage string) error {
	s.seen[stage]++
	if s.check != nil {
		s.check(c)
	}
	if stage == s.at && !s.once {
		s.once = true
		return s.action(c)
	}
	return nil
}
func (s *terminationPublicStore) SaveCheckpoint(c context.Context, id string, cp *store.Checkpoint) error {
	if e := s.hit(c, "checkpoint"); e != nil {
		return e
	}
	return s.RunStore.SaveCheckpoint(c, id, cp)
}
func (s *terminationPublicStore) UpdateRunStatusIfCoded(c context.Context, id string, status store.RunStatus, msg string, code store.FailureCode, from []store.RunStatus) (bool, error) {
	if e := s.hit(c, "status"); e != nil {
		return false, e
	}
	return s.RunStore.UpdateRunStatusIfCoded(c, id, status, msg, code, from)
}
func (s *terminationPublicStore) FailRunResumable(c context.Context, id string, cp *store.Checkpoint, msg string, code store.FailureCode) error {
	if e := s.hit(c, "resumable"); e != nil {
		return e
	}
	return s.RunStore.FailRunResumable(c, id, cp, msg, code)
}
func (s *terminationPublicStore) UpdateRunStatusCoded(c context.Context, id string, status store.RunStatus, msg string, code store.FailureCode) error {
	if e := s.hit(c, "fallback"); e != nil {
		return e
	}
	return s.RunStore.UpdateRunStatusCoded(c, id, status, msg, code)
}
func (s *terminationPublicStore) AppendEvent(c context.Context, id string, e store.Event) (*store.Event, error) {
	if e.Type == store.EventRunCancelled || e.Type == store.EventRunFailed {
		if err := s.hit(c, "event"); err != nil {
			return nil, err
		}
	}
	return s.RunStore.AppendEvent(c, id, e)
}

func terminationProbeWorkflow() *ir.Workflow {
	return &ir.Workflow{Name: "stop", Entry: "step", Nodes: map[string]ir.Node{"step": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "step"}}, "done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}}, Edges: []*ir.Edge{{From: "step", To: "done"}}}
}
func terminationFixture(t *testing.T, format store.ValueFormat) (store.RunStore, context.Context) {
	t.Helper()
	base := tmpStore(t)
	ctx := context.WithValue(store.WithTenant(t.Context(), "original-tenant"), terminationProbeKey{}, "original-value")
	r, err := store.CreateRunWithValueFormat(ctx, base, store.RunCreateSpec{ID: "stop", WorkflowName: "stop", Format: format})
	if err != nil {
		t.Fatal(err)
	}
	bound, err := store.BindRunValueFormat(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	return base, bound
}
func terminationReplacement(t *testing.T, base store.RunStore, bound context.Context, format store.ValueFormat) *store.Run {
	t.Helper()
	if err := base.DeleteRun(bound, "stop"); err != nil {
		t.Fatal(err)
	}
	p, ok := base.(interface {
		PruneDeletionMarkers(context.Context, time.Time) (int, error)
	})
	if !ok {
		t.Fatal("no pruner")
	}
	if _, err := p.PruneDeletionMarkers(store.WithTenant(t.Context(), "original-tenant"), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateRunWithValueFormat(t.Context(), base, store.RunCreateSpec{ID: "stop", WorkflowName: "successor", Format: format}); err != nil {
		t.Fatal(err)
	}
	got, err := base.LoadRun(t.Context(), "stop")
	if err != nil {
		t.Fatal(err)
	}
	return got
}
func checkTerminationContext(t *testing.T, c context.Context, format store.ValueFormat) {
	t.Helper()
	tenant, ok := store.TenantFromContext(c)
	if c.Err() != nil || !ok || tenant != "original-tenant" || c.Value(terminationProbeKey{}) != "original-value" {
		t.Fatalf("TERMINATION_CONTEXT_LOST: err=%v tenant=%q value=%v", c.Err(), tenant, c.Value(terminationProbeKey{}))
	}
	f, err := store.ValueFormatForWrite(c, "stop")
	if err != nil || f != format {
		t.Fatalf("TERMINATION_BINDING_LOST: %v %v", f, err)
	}
	if _, err := store.ValueFormatForWrite(c, "different"); !errors.Is(err, store.ErrValueFormatChanged) {
		t.Fatalf("TERMINATION_IDENTITY_LOST: %v", err)
	}
}

func TestTerminationPublicFormatMatrix(t *testing.T) {
	for _, format := range []store.ValueFormat{1, 2} {
		for _, stage := range []string{"checkpoint", "status", "event", "resumable"} {
			for _, swap := range []bool{false, true} {
				t.Run(fmt.Sprintf("format%d/%s/swap%v", format, stage, swap), func(t *testing.T) {
					base, bound := terminationFixture(t, format)
					ctx, cancel := context.WithCancelCause(bound)
					defer cancel(nil)
					s := &terminationPublicStore{RunStore: base, at: stage, seen: map[string]int{}}
					s.check = func(c context.Context) { checkTerminationContext(t, c, format) }
					var successor *store.Run
					s.action = func(context.Context) error {
						if swap {
							successor = terminationReplacement(t, base, bound, 3-format)
						}
						return nil
					}
					calls := 0
					x := newStubExecutor()
					x.on("step", func(map[string]any) (map[string]any, error) {
						calls++
						if stage == "resumable" {
							cancel(ErrRunInterrupted)
						} else {
							cancel(nil)
						}
						return nil, fmt.Errorf("node: %w", ctx.Err())
					})
					err := New(terminationProbeWorkflow(), s, x).Run(ctx, "stop", nil)
					got, readErr := base.LoadRun(t.Context(), "stop")
					if readErr != nil {
						t.Fatal(readErr)
					}
					if !s.once || calls != 1 {
						t.Fatalf("fixture missed stage: once=%v calls=%d", s.once, calls)
					}
					if swap {
						if !errors.Is(err, store.ErrValueFormatChanged) || errors.Is(err, ErrRunCancelled) || errors.Is(err, ErrRunInterrupted) || !reflect.DeepEqual(got, successor) {
							t.Fatalf("TERMINATION_SUCCESSOR_TOUCHED: err=%v before=%+v after=%+v", err, successor, got)
						}
						if stage == "checkpoint" && (s.seen["status"] != 0 || s.seen["event"] != 0) || stage == "status" && s.seen["event"] != 0 || stage == "resumable" && (s.seen["fallback"] != 0 || s.seen["event"] != 0) {
							t.Fatalf("TERMINATION_CALLBACK_AFTER_REFUSAL: %v", s.seen)
						}
					} else {
						want := store.RunStatusCancelled
						cause := ErrRunCancelled
						if stage == "resumable" {
							want = store.RunStatusFailedResumable
							cause = ErrRunInterrupted
						}
						if !errors.Is(err, cause) || got.Status != want || got.Checkpoint == nil {
							t.Fatalf("TERMINATION_HEALTHY_REFUSED: err=%v status=%s cp=%v", err, got.Status, got.Checkpoint != nil)
						}
					}
					t.Logf("format=%d stage=%s swap=%v err=%v status=%s callbacks=%v", format, stage, swap, err, got.Status, s.seen)
				})
			}
		}
	}
}

func TestTerminationPublicOrdinaryIO(t *testing.T) {
	for _, format := range []store.ValueFormat{1, 2} {
		for _, stage := range []string{"checkpoint", "status", "event", "resumable"} {
			t.Run(fmt.Sprintf("format%d/%s", format, stage), func(t *testing.T) {
				base, bound := terminationFixture(t, format)
				ctx, cancel := context.WithCancelCause(bound)
				defer cancel(nil)
				disk := errors.New("ordinary storage failure")
				s := &terminationPublicStore{RunStore: base, at: stage, seen: map[string]int{}, action: func(context.Context) error { return disk }}
				x := newStubExecutor()
				x.on("step", func(map[string]any) (map[string]any, error) {
					if stage == "resumable" {
						cancel(ErrRunInterrupted)
					} else {
						cancel(nil)
					}
					return nil, ctx.Err()
				})
				err := New(terminationProbeWorkflow(), s, x).Run(ctx, "stop", nil)
				got, readErr := base.LoadRun(t.Context(), "stop")
				if readErr != nil {
					t.Fatal(readErr)
				}
				if !s.once {
					t.Fatal("fixture missed stage")
				}
				switch stage {
				case "status":
					if !errors.Is(err, disk) || errors.Is(err, ErrRunCancelled) || got.Status != store.RunStatusRunning || s.seen["event"] != 0 {
						t.Fatalf("TERMINATION_UNPERSISTED_CANCEL: err=%v status=%s calls=%v", err, got.Status, s.seen)
					}
				case "checkpoint":
					if !errors.Is(err, ErrRunCancelled) || got.Status != store.RunStatusCancelled || got.Checkpoint != nil || s.seen["status"] != 1 || s.seen["event"] != 1 {
						t.Fatalf("TERMINATION_CHECKPOINT_POLICY: err=%v status=%s cp=%v calls=%v", err, got.Status, got.Checkpoint, s.seen)
					}
				case "event":
					if !errors.Is(err, ErrRunCancelled) || got.Status != store.RunStatusCancelled {
						t.Fatalf("TERMINATION_PERSISTED_CANCEL_LOST: %v %s", err, got.Status)
					}
				case "resumable":
					if errors.Is(err, ErrRunInterrupted) || errors.Is(err, ErrRunCancelled) || got.Status != store.RunStatusFailed || s.seen["fallback"] != 1 {
						t.Fatalf("TERMINATION_FALLBACK_POLICY: %v %s calls=%v", err, got.Status, s.seen)
					}
				}
				t.Logf("format=%d stage=%s err=%v status=%s callbacks=%v", format, stage, err, got.Status, s.seen)
			})
		}
	}
}

func TestTerminationPublicCASNoop(t *testing.T) {
	for _, format := range []store.ValueFormat{1, 2} {
		for _, terminal := range []store.RunStatus{store.RunStatusCancelled, store.RunStatusFinished, store.RunStatusFailed} {
			t.Run(fmt.Sprintf("format%d/%s", format, terminal), func(t *testing.T) {
				base, bound := terminationFixture(t, format)
				ctx, cancel := context.WithCancel(bound)
				defer cancel()
				s := &terminationPublicStore{RunStore: base, seen: map[string]int{}}
				x := newStubExecutor()
				x.on("step", func(map[string]any) (map[string]any, error) {
					if err := base.UpdateRunStatus(bound, "stop", terminal, "publisher reason"); err != nil {
						t.Fatal(err)
					}
					cancel()
					return nil, ctx.Err()
				})
				err := New(terminationProbeWorkflow(), s, x).Run(ctx, "stop", nil)
				got, readErr := base.LoadRun(t.Context(), "stop")
				if readErr != nil {
					t.Fatal(readErr)
				}
				if got.Status != terminal || got.Error != "publisher reason" || s.seen["status"] != 1 {
					t.Fatalf("TERMINATION_TERMINAL_OVERWRITTEN: %v %+v calls=%v", err, got, s.seen)
				}
				if terminal == store.RunStatusCancelled {
					if !errors.Is(err, ErrRunCancelled) || s.seen["event"] != 1 {
						t.Fatalf("TERMINATION_CANCELLED_NOOP: %v %v", err, s.seen)
					}
				} else if !errors.Is(err, ErrAdmissionStopped) || errors.Is(err, ErrRunCancelled) || s.seen["event"] != 0 {
					t.Fatalf("TERMINATION_OTHER_TERMINAL_CANCELLED: %v %v", err, s.seen)
				}
			})
		}
	}
}

func TestTerminationPublicHealthyRun(t *testing.T) {
	for _, format := range []store.ValueFormat{1, 2} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			base, bound := terminationFixture(t, format)
			calls := 0
			x := newStubExecutor()
			x.on("step", func(map[string]any) (map[string]any, error) { calls++; return map[string]any{"ok": true}, nil })
			err := New(terminationProbeWorkflow(), base, x, WithTerminationWriteTimeout(time.Nanosecond)).Run(bound, "stop", nil)
			got, readErr := base.LoadRun(t.Context(), "stop")
			if err != nil || readErr != nil || calls != 1 || got.Status != store.RunStatusFinished {
				t.Fatalf("TERMINATION_OPTION_AFFECTS_LIVE: %v %v calls=%d got=%+v", err, readErr, calls, got)
			}
		})
	}
}

func TestTerminationPublicWriteBudget(t *testing.T) {
	for _, budget := range []time.Duration{0, 2 * time.Second, -1} {
		for _, expire := range []bool{false, true} {
			if expire && budget < 0 {
				continue
			}
			t.Run(fmt.Sprintf("%s/expire%v", budget, expire), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					base, bound := terminationFixture(t, 2)
					ctx, cancel := context.WithCancel(bound)
					defer cancel()
					s := &terminationPublicStore{RunStore: base, at: "status", seen: map[string]int{}}
					s.action = func(c context.Context) error {
						checkTerminationContext(t, c, 2)
						deadline, bounded := c.Deadline()
						want := budget
						if want == 0 {
							want = 5 * time.Second
						}
						if bounded != (budget >= 0) || bounded && time.Until(deadline) != want {
							t.Fatalf("TERMINATION_BUDGET_CHANGED: bounded=%v remaining=%v want=%v", bounded, time.Until(deadline), want)
						}
						if expire {
							<-c.Done()
							return c.Err()
						}
						return nil
					}
					x := newStubExecutor()
					x.on("step", func(map[string]any) (map[string]any, error) { cancel(); return nil, ctx.Err() })
					err := New(terminationProbeWorkflow(), s, x, WithTerminationWriteTimeout(budget)).Run(ctx, "stop", nil)
					got, readErr := base.LoadRun(t.Context(), "stop")
					if readErr != nil {
						t.Fatal(readErr)
					}
					if !s.once {
						t.Fatal("fixture missed status")
					}
					if expire {
						if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrRunCancelled) || got.Status != store.RunStatusRunning || s.seen["event"] != 0 {
							t.Fatalf("TERMINATION_EXPIRED_WRITE_CLAIMED: err=%v status=%s calls=%v", err, got.Status, s.seen)
						}
					} else if !errors.Is(err, ErrRunCancelled) || got.Status != store.RunStatusCancelled {
						t.Fatalf("TERMINATION_BUDGET_REFUSED: %v %s", err, got.Status)
					}
				})
			})
		}
	}
}
