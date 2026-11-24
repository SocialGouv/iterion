package cli

import (
	"context"
	"errors"
	"testing"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

type autoResumeReadSpy struct {
	store.RunStore
	reads int
}

func (s *autoResumeReadSpy) LoadRun(context.Context, string) (*store.Run, error) {
	s.reads++
	return nil, errors.New("unexpected retry read")
}
func TestAutoResumeStopsTypedRefusalWithoutReadingSuccessor(t *testing.T) {
	t.Parallel()
	s := &autoResumeReadSpy{}
	original := store.ErrValueFormatChanged
	got := autoResumeLoop(t.Context(), nil, s, "run", autoResumeConfig{MaxAttempts: 1}, original, iterlog.Nop())
	if got != original || s.reads != 0 {
		t.Fatalf("error=%v reads=%d", got, s.reads)
	}
}
func TestAutoResumeReloadComparesOriginalBeforeRetry(t *testing.T) {
	t.Parallel()
	for _, format := range []store.ValueFormat{1, 2} {
		t.Run(string(rune('0'+format)), func(t *testing.T) {
			t.Parallel()
			s, err := store.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.CreateRunWithValueFormat(t.Context(), s, store.RunCreateSpec{ID: "run", Format: 3 - format}); err != nil {
				t.Fatal(err)
			}
			actualCtx, err := store.WithRunValueFormat(t.Context(), store.RunValueFormat{RunID: "run", Format: 3 - format})
			if err != nil {
				t.Fatal(err)
			}
			if err = s.UpdateRunStatus(actualCtx, "run", store.RunStatusFailedResumable, "earlier error"); err != nil {
				t.Fatal(err)
			}
			ctx, err := store.WithRunValueFormat(t.Context(), store.RunValueFormat{RunID: "run", Format: format})
			if err != nil {
				t.Fatal(err)
			}
			got := autoResumeLoop(ctx, nil, s, "run", autoResumeConfig{MaxAttempts: 1}, errors.New("earlier execution failed"), iterlog.Nop())
			if !errors.Is(got, store.ErrValueFormatChanged) {
				t.Fatalf("successor used in retry: %v", got)
			}
			events, err := s.LoadEvents(t.Context(), "run")
			if err != nil || len(events) != 0 {
				t.Fatalf("retry event emitted: %v %v", events, err)
			}
		})
	}
}
