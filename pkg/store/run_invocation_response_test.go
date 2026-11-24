package store

import (
	"context"
	"errors"
	"testing"
)

type preparedResponseStore struct {
	RunStore
	mode     string
	cancel   context.CancelFunc
	observed RunValueFormat
}

func (s *preparedResponseStore) CreateRunWithValueFormat(ctx context.Context, spec RunCreateSpec) (*Run, error) {
	if e := RequireRunValueFormat(ctx, spec.ID); e != nil {
		return nil, e
	}
	f, e := ValueFormatForWrite(ctx, spec.ID)
	if e != nil {
		return nil, e
	}
	s.observed = RunValueFormat{RunID: spec.ID, Format: f}
	r, e := s.CreateRun(ctx, spec.ID, spec.WorkflowName, spec.Inputs)
	if e != nil {
		return nil, e
	}
	if s.cancel != nil {
		s.cancel()
	}
	switch s.mode {
	case "wrongID":
		r.ID = "other"
	case "wrongFormat":
		r.FormatVersion = 2
	case "future":
		r.FormatVersion = 99
	case "commitError":
		return nil, errors.New("commit response unavailable")
	}
	return r, nil
}
func TestDeclaredBirthIntentSurvivesCancellationButNotInvalidCreator(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"normal", "wrongID", "wrongFormat", "future", "commitError"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			fs, e := New(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			spy := &preparedResponseStore{RunStore: fs, mode: mode, cancel: cancel}
			p, e := PrepareRunInvocation(ctx, spy, NewLegacyRunInvocation{Spec: RunCreateSpec{ID: "run"}})
			if spy.observed != (RunValueFormat{RunID: "run", Format: LegacyValueFormat}) {
				t.Fatalf("prebirth intent=%+v", spy.observed)
			}
			if mode == "normal" {
				if !errors.Is(e, context.Canceled) || !p.OwnedBirth || p.Context == nil {
					t.Fatalf("birth not retained: %+v %v", p, e)
				}
				if f, e := ValueFormatForWrite(context.WithoutCancel(p.Context), "run"); e != nil || f != LegacyValueFormat {
					t.Fatalf("original=%v %v", f, e)
				}
			} else {
				if e == nil || p.OwnedBirth || p.Context != nil {
					t.Fatalf("invalid creator adopted: %+v %v", p, e)
				}
				if mode != "commitError" && !IsValueFormatRefusal(e) {
					t.Fatalf("refusal erased by cancellation: %v", e)
				}
			}
		})
	}
}
