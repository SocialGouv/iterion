package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

type stopSwapStore struct {
	store.RunStore
	stage    string
	swap     func()
	fired    bool
	statuses int
	events   int
}

func (s *stopSwapStore) hit(stage string) {
	if stage == s.stage && !s.fired {
		s.fired = true
		s.swap()
	}
}
func (s *stopSwapStore) SaveCheckpoint(c context.Context, id string, cp *store.Checkpoint) error {
	s.hit("checkpoint")
	return s.RunStore.SaveCheckpoint(c, id, cp)
}
func (s *stopSwapStore) UpdateRunStatusIfCoded(c context.Context, id string, status store.RunStatus, msg string, code store.FailureCode, from []store.RunStatus) (bool, error) {
	s.statuses++
	s.hit("status")
	return s.RunStore.UpdateRunStatusIfCoded(c, id, status, msg, code, from)
}
func (s *stopSwapStore) FailRunResumable(c context.Context, id string, cp *store.Checkpoint, msg string, code store.FailureCode) error {
	s.hit("resumable")
	return s.RunStore.FailRunResumable(c, id, cp, msg, code)
}
func (s *stopSwapStore) AppendEvent(c context.Context, id string, e store.Event) (*store.Event, error) {
	if e.Type == store.EventRunCancelled || e.Type == store.EventRunFailed {
		s.events++
		s.hit("event")
	}
	return s.RunStore.AppendEvent(c, id, e)
}
func TestContextStopStoreRefusalPublic(t *testing.T) {
	for _, stage := range []string{"live", "checkpoint", "status", "event", "resumable"} {
		t.Run(stage, func(t *testing.T) {
			base := tmpStore(t)
			r, err := store.CreateRunWithValueFormat(t.Context(), base, store.RunCreateSpec{ID: "stop", Format: 1})
			if err != nil {
				t.Fatal(err)
			}
			bound, err := store.BindRunValueFormat(t.Context(), r)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancelCause(bound)
			defer cancel(nil)
			s := &stopSwapStore{RunStore: base, stage: stage}
			s.swap = func() {
				if err := base.DeleteRun(bound, "stop"); err != nil {
					t.Fatal(err)
				}
				prune, ok := base.(interface {
					PruneDeletionMarkers(context.Context, time.Time) (int, error)
				})
				if !ok {
					t.Fatal("no pruner")
				}
				if _, err := prune.PruneDeletionMarkers(bound, time.Now().Add(time.Hour)); err != nil {
					t.Fatal(err)
				}
				if _, err := store.CreateRunWithValueFormat(t.Context(), base, store.RunCreateSpec{ID: "stop", Format: 2}); err != nil {
					t.Fatal(err)
				}
			}
			x := newStubExecutor()
			x.on("step", func(map[string]any) (map[string]any, error) {
				if stage == "resumable" {
					cancel(ErrRunInterrupted)
				} else {
					cancel(nil)
				}
				return nil, fmt.Errorf("node: %w", ctx.Err())
			})
			wf := &ir.Workflow{Name: "stop", Entry: "step", Nodes: map[string]ir.Node{"step": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "step"}}}}
			err = New(wf, s, x).Run(ctx, "stop", nil)
			got, readErr := base.LoadRun(t.Context(), "stop")
			if readErr != nil {
				t.Fatal(readErr)
			}
			t.Logf("stage=%s swap=%v err=%v format=%d status=%s statuses=%d events=%d", stage, s.fired, err, got.FormatVersion, got.Status, s.statuses, s.events)
			if stage == "live" {
				if !errors.Is(err, ErrRunCancelled) || got.Status != store.RunStatusCancelled {
					t.Fatalf("LIVE_STOP_BROKEN: %v %+v", err, got)
				}
				return
			}
			if !s.fired {
				t.Fatal("fixture missed stage")
			}
			if !errors.Is(err, store.ErrValueFormatChanged) || errors.Is(err, ErrRunCancelled) || errors.Is(err, ErrRunInterrupted) || got.FormatVersion != 2 || got.Status != store.RunStatusRunning {
				t.Fatalf("STOP_STORE_REFUSAL_LOST: err=%v format=%d status=%s", err, got.FormatVersion, got.Status)
			}
			if stage == "checkpoint" && (s.statuses != 0 || s.events != 0) {
				t.Fatalf("SIDE_EFFECTS_AFTER_REFUSAL: status=%d events=%d", s.statuses, s.events)
			}
			if stage == "status" && s.events != 0 {
				t.Fatalf("EVENT_AFTER_REFUSAL: events=%d", s.events)
			}
		})
	}
}
