package supervise

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	iterlog "github.com/SocialGouv/iterion/pkg/log"
	"github.com/SocialGouv/iterion/pkg/store"
)

type cursorContextStore struct {
	store.RunStore
	seenContextErr error
}

func (s *cursorContextStore) SetWatcherCursor(ctx context.Context, _, _ string, _ store.WatcherCursor) error {
	s.seenContextErr = ctx.Err()
	return nil
}

type cursorlessRunStore struct{ store.RunStore }

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
	if c.cursor.LastAction == "intervene" {
		t.Fatalf("failed injection recorded a successful action: %+v", c.cursor)
	}
	c.evaluate("turn_boundary", true)
	if got := eval.calls(); got != 2 {
		t.Fatalf("failed injection was not retryable: calls=%d", got)
	}
}

func TestWatcherProgressFingerprintIgnoresDeliveryMetadata(t *testing.T) {
	a := &store.Event{
		Seq: 1, Timestamp: time.Unix(10, 0), Type: store.EventToolError,
		RunID: "run-a", BranchID: "branch", NodeID: "agent",
		Data: map[string]any{"error": "same failure", "tool": "Bash"},
	}
	b := &store.Event{
		Seq: 99, Timestamp: time.Unix(20, 0), Type: store.EventToolError,
		RunID: "run-b", BranchID: "branch", NodeID: "agent",
		Data:     map[string]any{"tool": "Bash", "error": "same failure"},
		TenantID: "tenant", LogOffset: 123, ActiveMs: 456,
	}
	if got, want := watcherProgressFingerprint(b), watcherProgressFingerprint(a); got != want {
		t.Fatalf("replayed evidence fingerprint = %q, want %q", got, want)
	}
	b.Data["error"] = "new failure"
	if watcherProgressFingerprint(b) == watcherProgressFingerprint(a) {
		t.Fatal("different event evidence produced the same fingerprint")
	}
}

func TestCoordinatorCountsRepeatedSemanticEvidence(t *testing.T) {
	c := newBareCoordinator(t, Spec{}, &stubEval{}, nil)
	c.ingest(&store.Event{Seq: 1, Type: store.EventAssistantText, NodeID: "agent", Data: map[string]any{"text": "stuck"}})
	c.ingest(&store.Event{Seq: 2, Type: store.EventAssistantText, NodeID: "agent", Data: map[string]any{"text": "stuck"}})
	if got := c.cursor.ConsecutiveNoProgress; got != 1 {
		t.Fatalf("consecutive no-progress count = %d, want 1", got)
	}
}

func TestCoordinatorSuppressesReplayedMonitorWithNewSequence(t *testing.T) {
	eval := &scriptedEval{decisions: []*Decision{{Intervene: false}}}
	c := newBareCoordinator(t, Spec{MaxEvals: 5}, eval, nil)
	first := &store.Event{Seq: 1, Type: store.EventToolError, NodeID: "agent", Data: map[string]any{"error": "stuck"}}
	c.ingest(first)
	c.evaluate("monitor matched: "+RenderEvent(first), true)

	replayed := &store.Event{Seq: 2, Type: first.Type, NodeID: first.NodeID, Data: map[string]any{"error": "stuck"}}
	c.ingest(replayed)
	c.evaluate("monitor matched: "+RenderEvent(replayed), true)
	if got := eval.calls(); got != 1 {
		t.Fatalf("replayed monitor evaluated %d times, want 1", got)
	}
}

func TestCoordinatorDoesNotSuppressNewMonitorEvidence(t *testing.T) {
	eval := &scriptedEval{decisions: []*Decision{{Intervene: false}, {Intervene: false}}}
	c := newBareCoordinator(t, Spec{MaxEvals: 5}, eval, nil)
	first := &store.Event{Seq: 1, Type: store.EventToolError, NodeID: "agent", Data: map[string]any{"error": "first"}}
	c.ingest(first)
	c.evaluate("monitor matched: "+RenderEvent(first), true)

	changed := &store.Event{Seq: 2, Type: first.Type, NodeID: first.NodeID, Data: map[string]any{"error": "second"}}
	c.ingest(changed)
	c.evaluate("monitor matched: "+RenderEvent(changed), true)
	if got := eval.calls(); got != 2 {
		t.Fatalf("new monitor evidence evaluated %d times, want 2", got)
	}
}

func TestWatcherCursorIDIsSafeForMongoUpdatePaths(t *testing.T) {
	id := watcherCursorID(Spec{Name: "review.$where", Watches: []string{"node.with.dot", "$node"}})
	if strings.ContainsAny(id, ".$") {
		t.Fatalf("watcher cursor id %q contains a Mongo path metacharacter", id)
	}
	if id != watcherCursorID(Spec{Name: "review.$where", Watches: []string{"node.with.dot", "$node"}}) {
		t.Fatal("watcher cursor id is not deterministic")
	}
}

func TestCoordinatorWarnsWhenCursorCapabilityIsHidden(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := iterlog.New(iterlog.LevelWarn, &logs)
	c := New(&fakeObserver{ch: make(chan *store.Event)}, &StoreInjector{Store: cursorlessRunStore{RunStore: st}}, "r1", Spec{Name: "watch"}, &stubEval{}, logger)
	c.ctx = context.Background()
	c.restoreCursor()
	if !strings.Contains(logs.String(), "restart replay suppression is disabled") {
		t.Fatalf("missing capability warning, logs=%q", logs.String())
	}
}

func TestCoordinatorPersistsCursorAfterParentCancellation(t *testing.T) {
	base, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := &cursorContextStore{RunStore: base}
	c := New(&fakeObserver{ch: make(chan *store.Event)}, &StoreInjector{Store: st}, "r1", Spec{Name: "watch"}, &stubEval{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	c.ctx = ctx
	cancel()
	c.persistCursor()
	if st.seenContextErr != nil {
		t.Fatalf("cursor save inherited cancelled context: %v", st.seenContextErr)
	}
}
