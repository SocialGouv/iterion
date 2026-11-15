package runtime

import (
	"context"
	"errors"
	"github.com/SocialGouv/iterion/pkg/store"
	"testing"
	"time"
)

type reviewRestampRefusalStore struct {
	store.RunStore
	t                     *testing.T
	mode                  string
	resumed, edges, saves int
	cancel                context.CancelFunc
	successor             *store.Run
}

func (s *reviewRestampRefusalStore) SaveRun(ctx context.Context, r *store.Run) error {
	if r.WorkflowSource == "r24-new-source" {
		fresh, e := s.LoadRun(ctx, r.ID)
		if e != nil || fresh.Status != store.RunStatusRunning {
			s.t.Fatalf("restamp before claim: %+v %v", fresh, e)
		}
		duplicate, e := s.UpdateRunStatusIf(ctx, r.ID, store.RunStatusRunning, "", []store.RunStatus{store.RunStatusPausedWaitingHuman})
		if e != nil || duplicate {
			s.t.Fatalf("duplicate pause claim admitted: %v %v", duplicate, e)
		}
		s.saves++
		if s.mode == "save-refusal" {
			return store.ErrValueFormatChanged
		}
	}
	return s.RunStore.SaveRun(ctx, r)
}
func (s *reviewRestampRefusalStore) AppendEvent(ctx context.Context, id string, event store.Event) (*store.Event, error) {
	if event.Type == store.EventEdgeSelected {
		s.edges++
	}
	persisted, err := s.RunStore.AppendEvent(ctx, id, event)
	if event.Type == store.EventRunResumed && err == nil {
		s.resumed++
		if s.mode == "cancel" {
			s.cancel()
		}
		if s.mode == "replacement" {
			actor := store.WithTenant(s.t.Context(), "reviewer-tenant")
			if e := s.DeleteRun(actor, id); e != nil {
				s.t.Fatal(e)
			}
			pruner := s.RunStore.(interface {
				PruneDeletionMarkers(context.Context, time.Time) (int, error)
			})
			if _, e := pruner.PruneDeletionMarkers(actor, time.Now().Add(time.Hour)); e != nil {
				s.t.Fatal(e)
			}
			var e error
			s.successor, e = store.CreateRunWithValueFormat(actor, s.RunStore, store.RunCreateSpec{ID: id, WorkflowName: "r24-successor", Format: store.ExactValueFormat})
			if e != nil {
				s.t.Fatal(e)
			}
		}
	}
	return persisted, err
}
func TestReviewRestampRefusal(t *testing.T) {
	for _, mode := range []string{"normal", "save-refusal", "replacement", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			base, ctx, _, _ := newResumeInvocationFixture(t, store.LegacyValueFormat)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			if e := base.PauseRun(ctx, "reviewer-resume", &store.Checkpoint{NodeID: "gate", InteractionID: "reviewer-question"}); e != nil {
				t.Fatal(e)
			}
			s := &reviewRestampRefusalStore{RunStore: base, t: t, mode: mode, cancel: cancel}
			e := New(minimalReviewWorkflow(), s, newStubExecutor(), WithWorkDir(t.TempDir()), WithSandboxOverride("none"))
			e.workflowSource = "r24-new-source"
			err := e.Resume(ctx, "reviewer-resume", map[string]any{reviewActionKey: "request_changes"})
			if s.resumed != 1 {
				t.Fatalf("no claim reached: count=%d err=%v", s.resumed, err)
			}
			switch mode {
			case "normal":
				if err != nil || s.edges != 1 || s.saves != 1 {
					t.Fatalf("control err=%v edges=%d saves=%d", err, s.edges, s.saves)
				}
			case "save-refusal", "replacement":
				if !errors.Is(err, store.ErrValueFormatChanged) || s.edges != 0 {
					t.Fatalf("restamp refusal lost err=%v edges=%d saves=%d", err, s.edges, s.saves)
				}
			case "cancel":
				if err == nil || s.edges != 0 {
					t.Fatalf("cancel ignored err=%v edges=%d", err, s.edges)
				}
			}
			if mode == "replacement" {
				r, err := base.LoadRun(store.WithTenant(t.Context(), "reviewer-tenant"), "reviewer-resume")
				if err != nil || r.WorkflowName != "r24-successor" || r.CASVersion != s.successor.CASVersion || r.Status != s.successor.Status {
					t.Fatalf("successor touched %+v %v", r, err)
				}
			}
		})
	}
}
