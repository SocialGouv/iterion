package runtime

import (
	"context"
	"errors"
	"fmt"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
	"time"
)

type resumeInvocationKey struct{}
type resumeInvocationEventStore struct {
	store.RunStore
	t         *testing.T
	format    store.ValueFormat
	deadline  time.Time
	mode      string
	cancel    context.CancelCauseFunc
	edges     int
	missing   []string
	appendErr error
	successor *store.Run
}

func (s *resumeInvocationEventStore) AppendEvent(ctx context.Context, id string, event store.Event) (*store.Event, error) {
	if event.Type == store.EventEdgeSelected {
		s.edges++
		if ctx == nil {
			s.missing = append(s.missing, "nil context")
		} else {
			f, e := store.ValueFormatForWrite(ctx, id)
			tenant, _ := store.TenantFromContext(ctx)
			deadline, ok := ctx.Deadline()
			_, wrong := store.ValueFormatForWrite(ctx, "not-this-run")
			if e != nil || f != s.format || tenant != "reviewer-tenant" || ctx.Value(resumeInvocationKey{}) != "reviewer-value" || !ok || !deadline.Equal(s.deadline) || !errors.Is(wrong, store.ErrValueFormatChanged) {
				s.missing = append(s.missing, fmt.Sprintf("format=%v err=%v tenant=%q value=%v deadline=%v/%v wrongID=%v", f, e, tenant, ctx.Value(resumeInvocationKey{}), deadline, ok, wrong))
			}
			if s.mode == "cancel" {
				cause := errors.New("reviewer original cancellation")
				s.cancel(cause)
				if !errors.Is(ctx.Err(), context.Canceled) || context.Cause(ctx) != cause {
					s.missing = append(s.missing, "original cancellation/cause detached")
				}
			}
		}
		if s.mode == "replacement" {
			actor := store.WithTenant(s.t.Context(), "reviewer-tenant")
			if err := s.DeleteRun(actor, id); err != nil {
				s.t.Fatal(err)
			}
			pruner, ok := s.RunStore.(interface {
				PruneDeletionMarkers(context.Context, time.Time) (int, error)
			})
			if !ok {
				s.t.Fatal("missing fixture pruner")
			}
			if _, err := pruner.PruneDeletionMarkers(actor, time.Now().Add(time.Hour)); err != nil {
				s.t.Fatal(err)
			}
			next := store.LegacyValueFormat
			if s.format == store.LegacyValueFormat {
				next = store.ExactValueFormat
			}
			var err error
			s.successor, err = store.CreateRunWithValueFormat(actor, s.RunStore, store.RunCreateSpec{ID: id, WorkflowName: "peer-successor", Format: next})
			if err != nil {
				s.t.Fatal(err)
			}
		}
	}
	persisted, err := s.RunStore.AppendEvent(ctx, id, event)
	if event.Type == store.EventEdgeSelected {
		s.appendErr = err
	}
	return persisted, err
}
func newResumeInvocationFixture(t *testing.T, format store.ValueFormat) (store.RunStore, context.Context, context.CancelCauseFunc, time.Time) {
	t.Helper()
	base := tmpStore(t)
	ctx := context.WithValue(store.WithTenant(t.Context(), "reviewer-tenant"), resumeInvocationKey{}, "reviewer-value")
	deadline := time.Now().Add(time.Hour)
	ctx, stop := context.WithDeadline(ctx, deadline)
	t.Cleanup(stop)
	ctx, cancel := context.WithCancelCause(ctx)
	t.Cleanup(func() { cancel(nil) })
	if _, err := store.CreateRunWithValueFormat(ctx, base, store.RunCreateSpec{ID: "reviewer-resume", WorkflowName: "review_test", Format: format}); err != nil {
		t.Fatal(err)
	}
	bound, err := store.WithRunValueFormat(ctx, store.RunValueFormat{RunID: "reviewer-resume", Format: format})
	if err != nil {
		t.Fatal(err)
	}
	return base, bound, cancel, deadline
}
func TestResumeReviewPreservesInvocationContext(t *testing.T) {
	for _, format := range []store.ValueFormat{store.LegacyValueFormat, store.ExactValueFormat} {
		for _, mode := range []string{"normal", "cancel", "replacement"} {
			t.Run(fmt.Sprintf("format%d/%s", format, mode), func(t *testing.T) {
				defer func() {
					if p := recover(); p != nil {
						t.Fatalf("PUBLIC_RESUME_CONTEXT_PANIC: %v", p)
					}
				}()
				base, ctx, cancel, deadline := newResumeInvocationFixture(t, format)
				if err := base.PauseRun(ctx, "reviewer-resume", &store.Checkpoint{NodeID: "gate", InteractionID: "reviewer-question"}); err != nil {
					t.Fatal(err)
				}
				s := &resumeInvocationEventStore{RunStore: base, t: t, format: format, deadline: deadline, mode: mode, cancel: cancel}
				e := New(minimalReviewWorkflow(), s, newStubExecutor(), WithWorkDir(t.TempDir()), WithSandboxOverride("none"))
				err := e.Resume(ctx, "reviewer-resume", map[string]any{reviewActionKey: "request_changes"})
				if s.edges != 1 || len(s.missing) > 0 {
					t.Fatalf("PUBLIC_RESUME_CONTEXT_LOST edges=%d missing=%v result=%v", s.edges, s.missing, err)
				}
				actor := store.WithTenant(t.Context(), "reviewer-tenant")
				r, loadErr := base.LoadRun(actor, "reviewer-resume")
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				switch mode {
				case "normal":
					if err != nil || r.Status != store.RunStatusFinished {
						t.Fatalf("RESUME_NOT_FINISHED: %v %+v", err, r)
					}
				case "cancel":
					if err == nil || r.Status == store.RunStatusFinished {
						t.Fatalf("CANCEL_IGNORED: %v status=%v", err, r.Status)
					}
				case "replacement":
					if !errors.Is(err, store.ErrValueFormatChanged) {
						t.Fatalf("RESUME_FORMAT_REFUSAL_LOST: %v", err)
					}
					if !errors.Is(s.appendErr, store.ErrValueFormatChanged) {
						t.Fatalf("ORIGINAL_FORMAT_RECAPTURED: %v", s.appendErr)
					}
					if r.WorkflowName != "peer-successor" || r.Status != s.successor.Status || r.CASVersion != s.successor.CASVersion {
						t.Fatalf("SUCCESSOR_CHANGED: %+v want %+v result=%v", r, s.successor, err)
					}
				}
				t.Logf("mode=%s format=%d original-context retained, event=%v result=%v status=%s", mode, format, s.appendErr, err, r.Status)
			})
		}
	}
}
