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

func TestEvalBudgetIsSpentPerRunNotPerProcess(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if _, err := st.CreateRun(context.Background(), "cursor-run", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	eval := &scriptedEval{decisions: []*Decision{{}}}
	inj := &StoreInjector{Store: st}
	spec := Spec{Name: "watch", Cooldown: time.Minute, MaxEvals: 1}

	c := New(&fakeObserver{ch: make(chan *store.Event)}, inj, "cursor-run", spec, eval, nil)
	c.ctx = context.Background()
	c.ingest(&store.Event{RunID: "cursor-run", Type: store.EventNodeStarted, NodeID: "agent", Seq: 1, Timestamp: time.Now().UTC()})
	c.evaluate("turn_boundary", true)
	if got := eval.calls(); got != 1 {
		t.Fatalf("first coordinator made %d evaluations, want 1", got)
	}

	c2 := New(&fakeObserver{ch: make(chan *store.Event)}, inj, "cursor-run", spec, eval, nil)
	c2.ctx = context.Background()
	c2.restoreCursor()
	// A DIFFERENT event, so the progress fingerprint (and therefore the
	// trigger fingerprint) is fresh: what must stop this evaluation is the
	// restored budget, not the duplicate-wake dedup.
	c2.ingest(&store.Event{RunID: "cursor-run", Type: store.EventNodeStarted, NodeID: "agent", Seq: 2, Timestamp: time.Now().UTC()})
	c2.evaluate("turn_boundary", true)
	if got := eval.calls(); got != 1 {
		t.Fatalf("restart granted a fresh eval budget: %d evaluations against MaxEvals=1", got)
	}
}

func TestWatcherCursorIDIsStableAcrossWatchOrder(t *testing.T) {
	base := watcherCursorID(Spec{Name: "persy", Watches: []string{"implement", "campaign"}})
	for _, spec := range []Spec{
		{Name: "persy", Watches: []string{"campaign", "implement"}},
		{Name: "persy", Watches: []string{"implement", "campaign", "implement"}},
	} {
		if got := watcherCursorID(spec); got != base {
			t.Fatalf("watches %v produced cursor id %q, want %q — a reordered --node flag must not lose the restart suppression",
				spec.Watches, got, base)
		}
	}
	if same := watcherCursorID(Spec{Name: "persy", Watches: []string{"implement"}}); same == base {
		t.Fatalf("a different watch set reused cursor id %q", same)
	}
	// Canonicalising must not mutate the caller's slice.
	watches := []string{"implement", "campaign"}
	_ = watcherCursorID(Spec{Name: "persy", Watches: watches})
	if watches[0] != "implement" {
		t.Fatalf("watcherCursorID reordered the caller's Watches: %v", watches)
	}
}

// cursorProbeInjector snapshots the run document at the instant Inject is
// called — the exact point a crash would leave a durable steering message
// behind an absent trigger fingerprint.
type cursorProbeInjector struct {
	StoreInjector
	cursorsAtInject map[string]store.WatcherCursor
}

func (i *cursorProbeInjector) Inject(ctx context.Context, runID, nodeID, text string) error {
	if run, err := i.Store.LoadRun(ctx, runID); err == nil && run != nil {
		i.cursorsAtInject = run.WatcherCursors
	}
	return i.StoreInjector.Inject(ctx, runID, nodeID, text)
}

func TestConsumedTriggerIsDurableBeforeTheSteeringMessageIs(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	if _, err := st.CreateRun(context.Background(), "cursor-run", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	inj := &cursorProbeInjector{StoreInjector: StoreInjector{Store: st}}
	eval := &scriptedEval{decisions: []*Decision{{Intervene: true, Message: "re-read the diff before continuing"}}}
	c := New(&fakeObserver{ch: make(chan *store.Event)}, inj, "cursor-run",
		Spec{Name: "watch", Cooldown: time.Minute}, eval, nil)
	c.ctx = context.Background()
	c.ingest(&store.Event{RunID: "cursor-run", Type: store.EventNodeStarted, NodeID: "agent", Seq: 1, Timestamp: time.Now().UTC()})
	c.evaluate("monitor matched: give-up marker", true)

	cursor, ok := inj.cursorsAtInject[c.cursorID]
	if !ok {
		t.Fatalf("run carried %#v when the steering message was enqueued, want cursor %q already durable",
			inj.cursorsAtInject, c.cursorID)
	}
	if cursor.LastTriggerFingerprint == "" {
		t.Fatalf("cursor %#v had no trigger fingerprint at enqueue time — a crash here replays the same correction", cursor)
	}
}

// ctxSensitiveStore refuses a read/write whose context is already spent,
// the way the Mongo twin does. The filesystem store takes `_ context.Context`
// and ignores it, so a plain store is structurally incapable of catching a
// cursor write issued on a cancelled context. It also records the tenant it
// saw, because the detach must drop the CANCELLATION and keep the VALUES —
// pkg/store/mongo panics on a tenant-less context.
type ctxSensitiveStore struct {
	store.RunStore
	savedTenant *string
}

func (s ctxSensitiveStore) LoadRun(ctx context.Context, id string) (*store.Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.RunStore.LoadRun(ctx, id)
}

func (s ctxSensitiveStore) SaveRun(ctx context.Context, r *store.Run) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if tenant, ok := store.TenantFromContext(ctx); ok {
		*s.savedTenant = tenant
	}
	return s.RunStore.SaveRun(ctx, r)
}

func TestPersistCursorOutlivesACancelledCoordinatorContextAndKeepsTenancy(t *testing.T) {
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	tenantCtx := store.WithTenant(context.Background(), "team-42")
	if _, err := st.CreateRun(tenantCtx, "cursor-run", "wf", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	savedTenant := ""
	inj := &StoreInjector{Store: ctxSensitiveStore{RunStore: st, savedTenant: &savedTenant}}
	c := New(&fakeObserver{ch: make(chan *store.Event)}, inj, "cursor-run",
		Spec{Name: "watch", Cooldown: time.Minute}, &scriptedEval{decisions: []*Decision{{}}}, nil)

	ctx, cancel := context.WithCancel(tenantCtx)
	c.ctx = ctx
	c.ingest(&store.Event{RunID: "cursor-run", Type: store.EventNodeStarted, NodeID: "agent", Seq: 1, Timestamp: time.Now().UTC()})
	// The run tore down: this is the state an evaluation cancelled at run
	// end defers into, and the one write that matters most for a restart.
	cancel()
	c.persistCursor()

	run, err := st.LoadRun(tenantCtx, "cursor-run")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if len(run.WatcherCursors) != 1 {
		t.Fatalf("watcher cursors = %#v, want the cursor persisted despite the spent coordinator context", run.WatcherCursors)
	}
	if savedTenant != "team-42" {
		t.Fatalf("detached cursor write carried tenant %q, want team-42 — dropping ctx VALUES panics the Mongo store", savedTenant)
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
