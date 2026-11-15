package runtime

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

type terminationCallbackScopeStore struct {
	store.RunStore
	a, b atomic.Int64
}

func (s *terminationCallbackScopeStore) AppendEvent(_ context.Context, id string, e store.Event) (*store.Event, error) {
	if id == "a" {
		s.a.Add(1)
		return nil, store.ErrValueFormatChanged
	}
	s.b.Add(1)
	return &e, nil
}
func TestTerminationScopedObserverIsolation(t *testing.T) {
	s := &terminationCallbackScopeStore{}
	e := New(terminationProbeWorkflow(), s, newStubExecutor())
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b"} {
		wg.Go(func() {
			w := e.newTerminationWrites(t.Context(), context.Canceled)
			defer w.Close()
			err := w.Do(func(c context.Context) error { return e.emitFailureIgnoringErrorForTest(c, id) })
			// The same event choke point rejects another callback in a refused scope,
			// while another scope on this Engine is unaffected.
			later := e.emit(w.ctx, id, store.EventRunFailed, "step", nil)
			if id == "a" && (!store.IsValueFormatRefusal(err) || !store.IsValueFormatRefusal(later)) {
				t.Errorf("SCOPE_REFUSAL_LOST: %v %v", err, later)
			}
			if id == "b" && (err != nil || later != nil) {
				t.Errorf("SCOPE_CONTAMINATED: %v %v", err, later)
			}
		})
	}
	wg.Wait()
	if s.a.Load() != 1 || s.b.Load() != 2 {
		t.Fatalf("CROSS_SCOPE_CALLBACKS a=%d b=%d", s.a.Load(), s.b.Load())
	}
	// A context without the private scope still performs its normal call.
	_ = e.emit(t.Context(), "a", store.EventRunFailed, "step", nil)
	if s.a.Load() != 2 {
		t.Fatal("UNSCOPED_POLICY_CHANGED")
	}
}

// The fake callback intentionally swallows the event result, like a legacy
// best-effort callback. The real event choke point above remains production.
func (e *Engine) emitFailureIgnoringErrorForTest(c context.Context, id string) error {
	_ = e.emit(c, id, store.EventRunFailed, "step", nil)
	return nil
}
func TestTerminationSettledErrorKeepsClassification(t *testing.T) {
	rt := &RuntimeError{Code: ErrCodeTimeout, Message: "stored timeout"}
	for _, cause := range []error{context.DeadlineExceeded, ErrRunCancelled, rt, errors.Join(rt, store.ErrValueFormatChanged)} {
		got := &terminationSettledError{cause: fmt.Errorf("wrapper: %w", cause)}
		if !errors.Is(got, cause) {
			t.Fatalf("SETTLED_IDENTITY_LOST: %v", got)
		}
		var actual *RuntimeError
		if errors.As(cause, &actual) {
			var target *RuntimeError
			if !errors.As(got, &target) || target != actual {
				t.Fatalf("SETTLED_CLASSIFICATION_LOST: %v", got)
			}
		}
	}
}
