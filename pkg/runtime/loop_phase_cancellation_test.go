package runtime

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/store"
)

type phaseErrorStore struct {
	store.RunStore
	at       store.EventType
	stop     context.CancelCauseFunc
	cause    error
	failure  func(error) error
	original error
	fired    bool
}

func (s *phaseErrorStore) AppendEvent(ctx context.Context, id string, e store.Event) (*store.Event, error) {
	if e.Type == s.at && !s.fired {
		s.fired = true
		s.stop(s.cause)
		s.original = s.failure(ctx.Err())
		return nil, s.original
	}
	return s.RunStore.AppendEvent(ctx, id, e)
}

func TestLoopPhaseCancellationPreservesPriorities(t *testing.T) {
	for _, at := range []store.EventType{store.EventNodeStarted, store.EventNodeFinished} {
		for _, kind := range []string{"cancel", "interrupt", "business", "ambiguous", "joined", "format", "ownership", "unrelated"} {
			t.Run(string(at)+"/"+kind, func(t *testing.T) {
				ctx, stop := context.WithCancelCause(t.Context())
				defer stop(nil)
				s := &phaseErrorStore{RunStore: tmpStore(t), at: at, stop: stop}
				switch kind {
				case "interrupt":
					s.cause = ErrRunInterrupted
					s.failure = func(e error) error { return fmt.Errorf("phase: %w", e) }
				case "cancel":
					s.failure = func(e error) error { return fmt.Errorf("phase: %w", e) }
				case "business":
					s.failure = func(e error) error { return &RuntimeError{Code: ErrCodeBudgetExceeded, Message: "business", Cause: e} }
				case "ambiguous":
					s.failure = func(e error) error {
						return &RuntimeError{Code: ErrCodeAmbiguousEffect, Message: "ambiguous", Cause: e}
					}
				case "joined":
					s.failure = func(e error) error { return errors.Join(e, errors.New("business sibling")) }
				case "format":
					s.failure = func(e error) error { return errors.Join(store.ErrValueFormatChanged, e) }
				case "ownership":
					s.failure = func(e error) error { return errors.Join(errCorrectionOwnershipLost, e) }
				case "unrelated":
					s.failure = func(error) error { return errors.New("ordinary I/O failure") }
				}
				wf := &ir.Workflow{Name: "phase", Entry: "step", Nodes: map[string]ir.Node{"step": &ir.AgentNode{BaseNode: ir.BaseNode{ID: "step"}}, "done": &ir.DoneNode{BaseNode: ir.BaseNode{ID: "done"}}}, Edges: []*ir.Edge{{From: "step", To: "done"}}}
				calls := 0
				x := newStubExecutor()
				x.on("step", func(map[string]any) (map[string]any, error) { calls++; return map[string]any{"ok": true}, nil })
				err := New(wf, s, x).Run(ctx, "phase", nil)
				r, readErr := s.LoadRun(t.Context(), "phase")
				if readErr != nil {
					t.Fatal(readErr)
				}
				if !s.fired {
					t.Fatal("fixture missed phase")
				}
				wantCalls := 0
				if at == store.EventNodeFinished {
					wantCalls = 1
				}
				if calls != wantCalls {
					t.Fatalf("nodes=%d want=%d", calls, wantCalls)
				}
				switch kind {
				case "cancel":
					if !errors.Is(err, ErrRunCancelled) || r.Status != store.RunStatusCancelled || r.Checkpoint == nil {
						t.Fatalf("PHASE_CANCELLATION_UNSETTLED: err=%v status=%s checkpoint=%v", err, r.Status, r.Checkpoint != nil)
					}
				case "interrupt":
					if !errors.Is(err, ErrRunInterrupted) || r.Status != store.RunStatusFailedResumable || r.Checkpoint == nil {
						t.Fatalf("PHASE_INTERRUPTION_UNSETTLED: err=%v status=%s checkpoint=%v", err, r.Status, r.Checkpoint != nil)
					}
				default:
					if !errors.Is(err, s.original) || errors.Is(err, ErrRunCancelled) || r.Status != store.RunStatusRunning {
						t.Fatalf("PHASE_ERROR_RECLASSIFIED: original=%v err=%v status=%s", s.original, err, r.Status)
					}
				}
				t.Logf("phase=%s class=%s nodes=%d err=%v status=%s", at, kind, calls, err, r.Status)
			})
		}
	}
}
