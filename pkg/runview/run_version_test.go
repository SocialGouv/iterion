package runview

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

type mutateBeforeSave struct {
	store.RunStore
	saves int
	at    int
}

func (s *mutateBeforeSave) SaveRun(ctx context.Context, run *store.Run) error {
	s.saves++
	if s.saves == s.at {
		if err := s.UpdateRunStatus(ctx, run.ID, store.RunStatusRunning, "concurrent resume"); err != nil {
			return err
		}
	}
	return s.RunStore.SaveRun(ctx, run)
}

func TestRenameDoesNotUndoConcurrentResume(t *testing.T) {
	svc, st, id := seedRun(t, linearBot, &store.Checkpoint{NodeID: "survey"}, store.RunStatusCancelled)
	svc.store = &mutateBeforeSave{RunStore: st, at: 1}
	if _, err := svc.RenameRunCtx(context.Background(), id, "renamed"); !errors.Is(err, store.ErrRunConflict) {
		t.Fatalf("rename=%v", err)
	}
	run, err := st.LoadRun(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunStatusRunning || run.Name == "renamed" {
		t.Fatalf("concurrent run overwritten: %+v", run)
	}
}

func TestRewindDoesNotOverwriteConcurrentResume(t *testing.T) {
	for _, at := range []int{1, 2} {
		t.Run(map[int]string{1: "claim", 2: "final-save"}[at], func(t *testing.T) {
			cp := &store.Checkpoint{NodeID: "plan", Outputs: map[string]map[string]any{"survey": {"value": "old"}}, ArtifactVersions: map[string]int{}}
			svc, st, id := seedRun(t, linearBot, cp, store.RunStatusCancelled)
			svc.store = &mutateBeforeSave{RunStore: st, at: at}
			_, err := svc.Rewind(context.Background(), RewindSpec{RunID: id, NodeID: "survey", KeepFiles: true})
			if !errors.Is(err, store.ErrRunConflict) {
				t.Fatalf("rewind=%v", err)
			}
			run, err := st.LoadRun(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != store.RunStatusRunning || run.Checkpoint.NodeID != "plan" {
				t.Fatalf("concurrent run overwritten: %+v", run)
			}
		})
	}
}
