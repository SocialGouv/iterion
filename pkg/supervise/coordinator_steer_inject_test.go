package supervise_test

// The free half of the runtime.supervisor split (#2793): the composed
// watch → decide → inject path with the steer decision STUBBED. One
// stand-in only — the Decision an LLMEvaluator would compose
// (Coordinator → GenerateObjectDirect in production). Everything around
// it is the real machinery: the run's event stream through the
// production transport (events.jsonl → broker), a seeded monitor wake,
// the crash-safe InjectOnce path through a real runview.Service, the
// file-backed inbox it lands in, the node-scoped drain a supervised
// node performs (pkg/backend/model's StoreInboxBinder), the full
// queued→delivered→consumed lifecycle, and the supervisor's durable
// cursor ending on one settled intervention. Zero LLM spend.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/backend/model"
	"github.com/SocialGouv/iterion/pkg/runview"
	"github.com/SocialGouv/iterion/pkg/store"
	"github.com/SocialGouv/iterion/pkg/supervise"
)

// steerStub is the single stand-in of the free half. Its first decision
// intervenes with a steering text that itself contains the monitor's
// marker, so the inbox echo (queued/delivered/consumed events carrying
// that text) would re-fire a naive coordinator; later calls observe, so
// even a pathological-slow run cannot produce a second injection.
type steerStub struct {
	mu     sync.Mutex
	inputs []supervise.EvalInput
}

func (s *steerStub) Evaluate(_ context.Context, in supervise.EvalInput) (*supervise.Decision, supervise.EvalUsage, error) {
	s.mu.Lock()
	s.inputs = append(s.inputs, in)
	calls := len(s.inputs)
	s.mu.Unlock()
	if calls == 1 {
		return &supervise.Decision{
			Intervene: true,
			Message:   "the failing marker means the test binary was never built — run the suite before editing",
			Reason:    "watched node reports an unbuilt test binary",
		}, supervise.EvalUsage{InputTokens: 12, OutputTokens: 7}, nil
	}
	return &supervise.Decision{Intervene: false}, supervise.EvalUsage{}, nil
}

func (s *steerStub) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inputs)
}

func (s *steerStub) firstInput() supervise.EvalInput {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inputs[0]
}

// waitForSteer polls cond until it holds or the deadline passes. A
// predicate's error never ends the poll, but the last one is carried
// into the fatal message: a timeout must tell "condition never held"
// apart from "the store is failing".
func waitForSteer(t *testing.T, cond func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ok, err := cond()
		if ok {
			return
		}
		if err != nil {
			lastErr = err
		}
		time.Sleep(5 * time.Millisecond)
	}
	if lastErr != nil {
		t.Fatalf("condition not met within deadline (last store error: %v)", lastErr)
	}
	t.Fatal("condition not met within deadline")
}

func TestCoordinatorSteersWatchedNodeThroughTheInbox(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}

	const runID = "run_steer_inject"
	if _, err := st.CreateRun(ctx, runID, "demo", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	svc, err := runview.NewService(dir)
	if err != nil {
		t.Fatalf("runview.NewService: %v", err)
	}

	spec := supervise.Spec{
		Name:    "watchdog",
		Watches: []string{"implement"},
		Monitors: []supervise.Monitor{
			{TextContains: "FAIL"},
		},
	}
	eval := &steerStub{}
	// The production pair: one service both observes and injects — the
	// shape New's own doc names for production callers.
	c := supervise.New(svc, svc, runID, spec, eval, nil)
	if c == nil {
		t.Fatal("New returned nil")
	}
	c.Start(ctx)
	defer c.Close()

	appendEvt := func(typ store.EventType, node string, data map[string]any) {
		t.Helper()
		if _, err := st.AppendEvent(ctx, runID, store.Event{Type: typ, NodeID: node, Data: data}); err != nil {
			t.Fatalf("append %s: %v", typ, err)
		}
	}
	appendEvt(store.EventNodeStarted, "implement", nil)
	appendEvt(store.EventAssistantText, "implement", map[string]any{"text": "running the suite"})
	appendEvt(store.EventAssistantText, "implement", map[string]any{"text": "go test: FAIL pkg/fixture"})

	// WATCH — the decision saw the watched node armed and the evidence
	// that woke it.
	waitForSteer(t, func() (bool, error) { return eval.calls() > 0, nil })
	in := eval.firstInput()
	if in.ActiveNode != "implement" {
		t.Fatalf("ActiveNode = %q, want the watched node", in.ActiveNode)
	}
	if !strings.Contains(in.WakeReason, "monitor matched") {
		t.Fatalf("WakeReason = %q, want the seeded monitor match", in.WakeReason)
	}
	sawMarker := false
	for _, line := range in.RecentEvents {
		if strings.Contains(line, "FAIL") {
			sawMarker = true
		}
	}
	if !sawMarker {
		t.Fatalf("the decision never saw the marker; recent events = %v", in.RecentEvents)
	}

	// INJECT — the production enqueue path landed the steering in the
	// real file-backed inbox, scoped to the watched node and framed as
	// the supervisor's.
	waitForSteer(t, func() (bool, error) {
		pending, err := st.LoadPendingQueuedMessages(ctx, runID)
		if err != nil {
			return false, err
		}
		return len(pending) == 1, nil
	})
	pending, err := st.LoadPendingQueuedMessages(ctx, runID)
	if err != nil {
		t.Fatalf("LoadPendingQueuedMessages: %v", err)
	}
	msg := pending[0]
	if msg.NodeID != "implement" {
		t.Fatalf("queued message node = %q, want the watched node (a run-scoped or mis-scoped steer leaks into the next node)", msg.NodeID)
	}
	if !strings.HasPrefix(msg.Text, "[supervisor watchdog] ") {
		t.Fatalf("steering text lacks the supervisor framing: %q", msg.Text)
	}
	if !strings.Contains(msg.Text, "test binary") {
		t.Fatalf("steering text lost the stub decision's message: %q", msg.Text)
	}

	// The queued event is on the run's own stream — the studio inbox
	// fan-out of the REAL QueueMessage, not a test-side append. It is
	// appended just after the inbox row it announces, so wait for it
	// instead of reading once: the inbox row becoming visible does not
	// prove its event already landed.
	waitForSteer(t, func() (bool, error) {
		events, err := st.LoadEvents(ctx, runID)
		if err != nil {
			return false, err
		}
		return countSteerEvents(events, store.EventUserMessageQueued) == 1, nil
	})
	events, err := st.LoadEvents(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if n := countSteerEvents(events, store.EventUserMessageQueued); n != 1 {
		t.Fatalf("user_message_queued events = %d, want 1", n)
	}

	// DELIVER — negative scoping first: while another node executes, the
	// steering waits; a message tagged for the watched node must not leak
	// into its drain.
	var published []store.Event
	binder := &model.StoreInboxBinder{
		Store: st,
		Publish: func(e store.Event) {
			published = append(published, e)
		},
	}
	otherHook := binder.Bind(model.WithNodeID(ctx, "review"), runID)
	if otherHook == nil {
		t.Fatal("Bind returned nil hook for a configured binder")
	}
	if got := otherHook.Drain(ctx); len(got) != 0 {
		t.Fatalf("a watched-node-scoped steer leaked into another node's drain: %v", got)
	}
	stillPending, err := st.LoadPendingQueuedMessages(ctx, runID)
	if err != nil || len(stillPending) != 1 {
		t.Fatalf("after the wrong node's drain pending = %d (err %v), want 1 — the steer must wait for its node", len(stillPending), err)
	}

	// The watched node's drain sees it, once.
	hook := binder.Bind(model.WithNodeID(ctx, "implement"), runID)
	if hook == nil {
		t.Fatal("Bind returned nil hook for the watched node")
	}
	got := hook.Drain(ctx)
	if len(got) != 1 || !strings.Contains(got[0], "test binary") {
		t.Fatalf("watched node's drain = %v, want the steering text", got)
	}
	if got := hook.Drain(ctx); len(got) != 0 {
		t.Fatalf("second drain redelivered %v, want empty", got)
	}
	if n := countSteerEvents(published, store.EventUserMessageDelivered); n != 1 {
		t.Fatalf("delivered events = %d, want 1", n)
	}

	// CONSUME — the node's turn closes the inbox lifecycle.
	hook.Consume(ctx)
	if n := countSteerEvents(published, store.EventUserMessageConsumed); n != 1 {
		t.Fatalf("consumed events = %d, want 1", n)
	}
	all, err := st.ListQueuedMessages(ctx, runID)
	if err != nil {
		t.Fatalf("ListQueuedMessages: %v", err)
	}
	for _, m := range all {
		if m.Status != store.QueuedMessageStatusConsumed {
			t.Errorf("inbox row %s status = %s, want consumed", m.ID, m.Status)
		}
	}

	// STATE MACHINE — the supervisor's durable cursor ends on one settled
	// intervention: the reservation consumed, the action named, the eval
	// timestamps stamped.
	var cursor store.WatcherCursor
	waitForSteer(t, func() (bool, error) {
		run, err := st.LoadRun(ctx, runID)
		if err != nil {
			return false, err
		}
		if len(run.WatcherCursors) != 1 {
			return false, nil
		}
		for _, cur := range run.WatcherCursors {
			cursor = cur
		}
		return cursor.InterventionSequence == 1 && cursor.PendingInterventionID == "" &&
			cursor.LastAction == "intervene", nil
	})
	if !strings.HasPrefix(cursor.WatcherID, "supervisor:") {
		t.Errorf("cursor watcher id = %q, want the supervisor prefix", cursor.WatcherID)
	}
	if cursor.LastEvaluationAt == nil || cursor.NextEvaluationAt == nil {
		t.Errorf("cursor evaluation stamps missing: last=%v next=%v", cursor.LastEvaluationAt, cursor.NextEvaluationAt)
	}
	if cursor.LastTriggerFingerprint == "" {
		t.Error("cursor carries no trigger fingerprint — a coordinator restart could replay the same steer")
	}

	// QUIESCENCE — the steering's own echo comes back through the same
	// transport carrying the marker; it must wake nobody and inject
	// nothing. The text deliberately contains the monitor's marker, so a
	// naive coordinator would re-fire here. What this phase proves is the
	// composed property — one settled intervention, no further model turn
	// — whose enforcement is layered (the run-loop inbox skip, the
	// ingest-side progress skip, the replay fingerprint, the cooldown)
	// and attributable to no single guard; each guard's own witness lives
	// in coordinator_inboxecho_test.go, shaped so the others cannot stand
	// in for it.
	time.Sleep(250 * time.Millisecond)
	if got := eval.calls(); got != 1 {
		t.Fatalf("the steer's own inbox echo woke the supervisor: evals=%d, want 1", got)
	}
	if pending, err := st.LoadPendingQueuedMessages(ctx, runID); err != nil || len(pending) != 0 {
		t.Fatalf("post-quiescence pending = %d (err %v), want 0 — the echo injected again", len(pending), err)
	}
	events, err = st.LoadEvents(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if n := countSteerEvents(events, store.EventUserMessageQueued); n != 1 {
		t.Fatalf("user_message_queued events after quiescence = %d, want 1", n)
	}
	if n := countSteerEvents(events, store.EventUserMessageDelivered); n != 1 {
		t.Fatalf("user_message_delivered events after quiescence = %d, want 1", n)
	}
	if n := countSteerEvents(events, store.EventUserMessageConsumed); n != 1 {
		t.Fatalf("user_message_consumed events after quiescence = %d, want 1", n)
	}
}

// countSteerEvents counts events of one type across the shapes the two
// transports hand over: LoadEvents yields pointers, the binder's publish
// callback yields values.
func countSteerEvents[T any](events []T, typ store.EventType) int {
	n := 0
	for _, e := range events {
		switch v := any(e).(type) {
		case *store.Event:
			if v != nil && v.Type == typ {
				n++
			}
		case store.Event:
			if v.Type == typ {
				n++
			}
		}
	}
	return n
}
