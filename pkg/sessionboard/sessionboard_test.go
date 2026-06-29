package sessionboard

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestApplyDecision(t *testing.T) {
	base := Spec{Version: 3, Widgets: []Widget{
		{ID: "w1", Kind: KindNote, Title: "Status", Props: map[string]any{"text": "starting"}},
	}}

	t.Run("upsert new + update existing bumps version", func(t *testing.T) {
		next, changed := ApplyDecision(base, BoardDecision{Upsert: []Widget{
			{ID: "w1", Kind: KindNote, Title: "Status", Props: map[string]any{"text": "building"}},
			{ID: "w2", Kind: KindMetric, Title: "Files", Props: map[string]any{"value": 3}},
		}})
		if !changed {
			t.Fatal("expected changed")
		}
		if next.Version != 4 {
			t.Errorf("version = %d, want 4", next.Version)
		}
		if len(next.Widgets) != 2 {
			t.Fatalf("widgets = %d, want 2", len(next.Widgets))
		}
		if next.Widgets[0].Props["text"] != "building" {
			t.Errorf("w1 not updated: %+v", next.Widgets[0])
		}
	})

	t.Run("identical upsert is a no-op", func(t *testing.T) {
		next, changed := ApplyDecision(base, BoardDecision{Upsert: []Widget{
			{ID: "w1", Kind: KindNote, Title: "Status", Props: map[string]any{"text": "starting"}},
		}})
		if changed {
			t.Errorf("expected no change, got version %d", next.Version)
		}
		if next.Version != 3 {
			t.Errorf("version regressed/advanced: %d", next.Version)
		}
	})

	t.Run("remove drops a widget", func(t *testing.T) {
		next, changed := ApplyDecision(base, BoardDecision{Remove: []string{"w1"}})
		if !changed || len(next.Widgets) != 0 {
			t.Fatalf("remove failed: changed=%v widgets=%d", changed, len(next.Widgets))
		}
	})

	t.Run("unknown kind / empty id is dropped", func(t *testing.T) {
		_, changed := ApplyDecision(Spec{}, BoardDecision{Upsert: []Widget{
			{ID: "x", Kind: "totally_made_up"},
			{ID: "", Kind: KindNote},
		}})
		if changed {
			t.Error("expected invalid widgets to be dropped (no change)")
		}
	})

	t.Run("remove then upsert same id in one decision", func(t *testing.T) {
		next, changed := ApplyDecision(base, BoardDecision{
			Remove: []string{"w1"},
			Upsert: []Widget{{ID: "w1", Kind: KindMetric, Title: "n", Props: map[string]any{"value": 1}}},
		})
		if !changed || len(next.Widgets) != 1 || next.Widgets[0].Kind != KindMetric {
			t.Fatalf("remove+re-add failed: %+v", next.Widgets)
		}
	})
}

func TestFileStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Missing spec → zero value, no error.
	got, err := st.Load("run_x")
	if err != nil {
		t.Fatalf("load missing: %v", err)
	}
	if got.Version != 0 || len(got.Widgets) != 0 {
		t.Errorf("missing spec not zero-value: %+v", got)
	}

	spec := Spec{Version: 2, UpdatedSeq: 9, Widgets: []Widget{
		{ID: "w1", Kind: KindProgress, Title: "Migration", Props: map[string]any{"value": 4, "max": 12}},
	}}
	if err := st.Save("run_x", spec); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err = st.Load("run_x")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.Version != 2 || got.UpdatedSeq != 9 || len(got.Widgets) != 1 || got.Widgets[0].ID != "w1" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

// fakeEvaluator returns a queued decision per call.
type fakeEvaluator struct {
	mu    sync.Mutex
	decs  []BoardDecision
	calls int
}

func (f *fakeEvaluator) Evaluate(_ context.Context, _ EvalInput) (*BoardDecision, EvalUsage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if len(f.decs) == 0 {
		return &BoardDecision{}, EvalUsage{}, nil
	}
	d := f.decs[0]
	f.decs = f.decs[1:]
	return &d, EvalUsage{InputTokens: 10, OutputTokens: 5}, nil
}

// fakeEmitter records published specs.
type fakeEmitter struct {
	mu    sync.Mutex
	specs []Spec
}

func (e *fakeEmitter) Publish(_ context.Context, _ string, spec Spec) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.specs = append(e.specs, spec)
	return nil
}

func (e *fakeEmitter) last() (Spec, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.specs) == 0 {
		return Spec{}, false
	}
	return e.specs[len(e.specs)-1], true
}

// staticObserver streams a fixed event slice then keeps the channel open
// until ctx cancel (so the coordinator doesn't self-close before we read).
type staticObserver struct{ events []*store.Event }

func (o staticObserver) ObserveRun(ctx context.Context, _ string) (<-chan *store.Event, func(), error) {
	ch := make(chan *store.Event)
	go func() {
		for _, e := range o.events {
			select {
			case ch <- e:
			case <-ctx.Done():
				return
			}
		}
		<-ctx.Done()
	}()
	return ch, func() {}, nil
}

func TestCoordinatorAppliesDiffOnTurnBoundary(t *testing.T) {
	now := time.Now()
	ev := func(seq int64, typ store.EventType, node string) *store.Event {
		return &store.Event{Seq: seq, Type: typ, NodeID: node, Timestamp: now.Add(time.Duration(seq) * time.Millisecond)}
	}
	obs := staticObserver{events: []*store.Event{
		ev(1, store.EventNodeStarted, "implement"),
		ev(2, store.EventNodeFinished, "implement"),
	}}
	eval := &fakeEvaluator{decs: []BoardDecision{{
		Upsert: []Widget{{ID: "narrative", Kind: KindNote, Title: "Session", Props: map[string]any{"text": "Implementing the feature"}}},
	}}}
	emit := &fakeEmitter{}

	// Tiny cooldown/debounce window so the test is fast.
	c := New(obs, emit, "run_1", Config{Cooldown: time.Millisecond, MaxEvals: 5}, eval, nil)
	if c == nil {
		t.Fatal("New returned nil")
	}
	// The coordinator started AFTER `now`; events are stamped around now,
	// so make them look fresh by shifting startedAt back via Start timing:
	// start, then the events (stamped slightly in the future) are > startedAt.
	c.startedAt = now.Add(-time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.ctx, c.cancel = context.WithCancel(ctx)
	go c.run()

	// Evaluation fires turnDebounce (3s) after the last turn boundary.
	deadline := time.After(6 * time.Second)
	for {
		if spec, ok := emit.last(); ok {
			if len(spec.Widgets) == 1 && spec.Widgets[0].ID == "narrative" {
				break
			}
		}
		select {
		case <-deadline:
			t.Fatalf("board not updated in time (eval calls=%d)", eval.calls)
		case <-time.After(20 * time.Millisecond):
		}
	}
	c.Close()
}
