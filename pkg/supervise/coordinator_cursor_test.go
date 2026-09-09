package supervise

import (
	"context"
	"errors"
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

func TestCoordinatorNilDecisionIsRetryable(t *testing.T) {
	eval := &scriptedEval{decisions: []*Decision{nil, {Intervene: false}}}
	c := newBareCoordinator(t, Spec{MaxEvals: 5}, eval, nil)
	c.ingest(&store.Event{Type: store.EventNodeStarted, NodeID: "agent", Timestamp: time.Now().UTC()})

	c.evaluate("turn_boundary", true)
	if c.cursor.LastTriggerFingerprint != "" {
		t.Fatal("nil decision consumed the durable trigger")
	}
	c.evaluate("turn_boundary", true)
	if got := eval.calls(); got != 2 {
		t.Fatalf("nil decision was not retried: calls=%d", got)
	}
}

type failingInjector struct{ err error }

func (f *failingInjector) Inject(context.Context, string, string, string) error { return f.err }

func TestCoordinatorFailedInjectionDoesNotCommitIntervention(t *testing.T) {
	eval := &scriptedEval{decisions: []*Decision{{Intervene: true, Message: "fix it"}}}
	c := newBareCoordinator(t, Spec{MaxEvals: 5}, eval, &failingInjector{err: errors.New("inbox unavailable")})
	c.ingest(&store.Event{Type: store.EventNodeStarted, NodeID: "agent", Timestamp: time.Now().UTC()})

	c.evaluate("turn_boundary", true)
	if c.cursor.LastTriggerFingerprint != "" {
		t.Fatal("failed injection consumed the durable trigger")
	}
	if c.cursor.LastAction == "intervene" || c.cursor.LastActionFingerprint != "" {
		t.Fatalf("failed injection recorded a successful action: %+v", c.cursor)
	}
	c.evaluate("turn_boundary", true)
	if got := eval.calls(); got != 2 {
		t.Fatalf("failed injection was not retryable: calls=%d", got)
	}
}
