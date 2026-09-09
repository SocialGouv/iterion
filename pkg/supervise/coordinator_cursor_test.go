package supervise

import (
	"context"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestCoordinatorPersistsCursorAndSuppressesDuplicateWake(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if _, err := st.CreateRun(context.Background(), "cursor-run", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	eval := &scriptedEval{decisions: []*Decision{{Intervene: false}}}
	inj := &StoreInjector{Store: st}
	c := New(&fakeObserver{ch: make(chan *store.Event)}, inj, "cursor-run", Spec{Name: "watch", Cooldown: time.Minute}, eval, nil)
	c.ctx = context.Background()
	c.ingest(&store.Event{RunID: "cursor-run", Type: store.EventNodeStarted, NodeID: "agent", Timestamp: time.Now().UTC()})
	c.evaluate("turn_boundary", true)

	run, err := st.LoadRun(context.Background(), "cursor-run")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if len(run.WatcherCursors) != 1 {
		t.Fatalf("watcher cursors = %#v, want one durable cursor", run.WatcherCursors)
	}

	c2 := New(&fakeObserver{ch: make(chan *store.Event)}, inj, "cursor-run", Spec{Name: "watch", Cooldown: time.Minute}, eval, nil)
	c2.ctx = context.Background()
	c2.restoreCursor()
	c2.evaluate("turn_boundary", true)
	if got := eval.calls(); got != 1 {
		t.Fatalf("duplicate wake evaluated %d times after restart, want 1", got)
	}
}

// countingPayload counts how often it is JSON-marshalled. RenderEvent
// marshals evt.Data, so this makes "how many times did ingest render this
// event" a deterministic assertion instead of a code-reading exercise.
type countingPayload struct{ n *int }

func (p countingPayload) MarshalJSON() ([]byte, error) {
	*p.n++
	return []byte(`"payload"`), nil
}

func TestIngestRendersOnceAndLeavesTheCallersEventAlone(t *testing.T) {
	renders := 0
	evt := &store.Event{
		RunID: "cursor-run", Type: store.EventNodeStarted, NodeID: "agent", Seq: 7,
		Data: map[string]any{"payload": countingPayload{n: &renders}},
	}
	c := New(&fakeObserver{ch: make(chan *store.Event)}, &recordInjector{}, "cursor-run",
		Spec{Name: "watch", Cooldown: time.Minute}, &scriptedEval{decisions: []*Decision{{}}}, nil)
	c.ingest(evt)

	if renders != 1 {
		t.Fatalf("ingest rendered the event %d times, want 1 — RenderEvent marshals Data on every event of every supervised run", renders)
	}
	if !evt.Timestamp.IsZero() {
		t.Fatalf("ingest stamped %v onto the caller's event; it folds a borrowed value and must derive the progress time locally", evt.Timestamp)
	}
}
