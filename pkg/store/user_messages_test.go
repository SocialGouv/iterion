package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// Node-scoped delivery: a message tagged for node "B" must NOT be
// drained while node "A" is active, but a run-scoped (untagged) message
// always drains. When "B" becomes active its message drains; an empty
// active node reproduces the legacy run-scoped behaviour (drains all).
func TestDrainPendingForNodeScoping(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-nodescope"

	mk := func(id, node string) {
		t.Helper()
		if err := s.AppendQueuedMessage(ctx, runID, QueuedUserMessage{
			ID: id, Text: "t-" + id, NodeID: node, QueuedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("AppendQueuedMessage %s: %v", id, err)
		}
	}
	mk("run1", "")  // run-scoped
	mk("forA", "A") // tagged for node A
	mk("forB", "B") // tagged for node B

	// Node A active: drains run-scoped + A-tagged, leaves B-tagged queued.
	texts, ids, err := DrainPendingForNode(ctx, s, nil, runID, "A")
	if err != nil {
		t.Fatalf("DrainPendingForNode(A): %v", err)
	}
	if len(ids) != 2 || !contains(ids, "run1") || !contains(ids, "forA") {
		t.Fatalf("node A drained %v (texts %v); want run1+forA", ids, texts)
	}
	pending, _ := s.LoadPendingQueuedMessages(ctx, runID)
	if len(pending) != 1 || pending[0].ID != "forB" {
		t.Fatalf("after A drain, pending=%+v; want only forB", pending)
	}

	// Node B active: now its message drains.
	_, ids, err = DrainPendingForNode(ctx, s, nil, runID, "B")
	if err != nil {
		t.Fatalf("DrainPendingForNode(B): %v", err)
	}
	if len(ids) != 1 || ids[0] != "forB" {
		t.Fatalf("node B drained %v; want forB", ids)
	}
}

// An empty active node drains every queued message regardless of tag —
// the run-scoped fallback used by operator-typed chatbox messages and
// the pauseAtHuman drainer.
func TestDrainPendingForNodeEmptyDrainsAll(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-nodescope-all"

	for _, n := range []struct{ id, node string }{{"x", ""}, {"y", "A"}, {"z", "B"}} {
		if err := s.AppendQueuedMessage(ctx, runID, QueuedUserMessage{
			ID: n.id, Text: "t", NodeID: n.node, QueuedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("append %s: %v", n.id, err)
		}
	}
	_, ids, err := DrainPendingForNode(ctx, s, nil, runID, "")
	if err != nil {
		t.Fatalf("DrainPendingForNode(\"\"): %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("empty-node drain got %d ids %v; want all 3", len(ids), ids)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// The inbox doorbell must advance for appends made through a DIFFERENT
// store handle on the same directory — the cross-process case a
// supervisor steering a run from a separate process relies on. Without
// the file-size term, handle B's version would never move and its drain
// would fast-skip A's message forever.
func TestQueuedInboxVersionSeesCrossHandleAppend(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	const runID = "run-xhandle"

	a, err := New(dir)
	if err != nil {
		t.Fatalf("New a: %v", err)
	}
	b, err := New(dir)
	if err != nil {
		t.Fatalf("New b: %v", err)
	}

	before := b.QueuedInboxVersion(runID)
	if err := a.AppendQueuedMessage(ctx, runID, QueuedUserMessage{ID: "m1", Text: "from another process"}); err != nil {
		t.Fatalf("append via a: %v", err)
	}
	after := b.QueuedInboxVersion(runID)
	if after <= before {
		t.Fatalf("handle B version did not advance after cross-handle append: before=%d after=%d", before, after)
	}
	// And B can actually drain the message A wrote.
	pending, err := b.LoadPendingQueuedMessages(ctx, runID)
	if err != nil || len(pending) != 1 || pending[0].ID != "m1" {
		t.Fatalf("handle B pending=%+v err=%v; want m1", pending, err)
	}
}

// A torn line (crash/OOM/ENOSPC mid-append) must not brick the inbox:
// the valid records still load. Before the fix, loadLatestQueuedMessages
// returned an error on the first bad line, so ListQueuedMessages /
// LoadPendingQueuedMessages / UpdateQueuedMessageStatus all 500'd for
// the rest of the run's life.
func TestLoadQueuedMessagesToleratesTornLine(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-inbox"

	if err := s.AppendQueuedMessage(ctx, runID, QueuedUserMessage{
		ID:       "m1",
		Text:     "hello",
		QueuedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("AppendQueuedMessage: %v", err)
	}

	// Simulate a crash mid-append: a partial JSON record with no
	// trailing newline at EOF.
	path, err := s.userMessagesPath(runID)
	if err != nil {
		t.Fatalf("userMessagesPath: %v", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := f.WriteString(`{"id":"m2","run_id":"run-inbox","sta`); err != nil {
		t.Fatalf("write torn line: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	msgs, err := s.ListQueuedMessages(ctx, runID)
	if err != nil {
		t.Fatalf("ListQueuedMessages after torn write: %v", err)
	}
	if len(msgs) != 1 || msgs[0].ID != "m1" {
		t.Fatalf("got %d messages %+v; want exactly m1", len(msgs), msgs)
	}

	pending, err := s.LoadPendingQueuedMessages(ctx, runID)
	if err != nil {
		t.Fatalf("LoadPendingQueuedMessages after torn write: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "m1" {
		t.Fatalf("got %d pending %+v; want exactly m1", len(pending), pending)
	}
}

// Wholesale corruption (not a single torn tail) must still fail loudly,
// matching the events.jsonl policy — a single skip is benign, mass
// garbage is not.
func TestLoadQueuedMessagesFailsLoudOnMassCorruption(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-garbage"

	if err := s.AppendQueuedMessage(ctx, runID, QueuedUserMessage{
		ID:       "m1",
		Text:     "ok",
		QueuedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("AppendQueuedMessage: %v", err)
	}
	path, err := s.userMessagesPath(runID)
	if err != nil {
		t.Fatalf("userMessagesPath: %v", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for i := 0; i < 8; i++ {
		if _, err := f.WriteString("this is not json\n"); err != nil {
			t.Fatalf("write garbage: %v", err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if _, err := s.ListQueuedMessages(ctx, runID); !errors.Is(err, ErrEventsCorrupted) {
		t.Fatalf("ListQueuedMessages on mass corruption = %v; want ErrEventsCorrupted", err)
	}
}

// InboxEventFor is the single wire-shape builder shared by all three
// emission sites, so a mis-mapped or dropped field would desync the
// studio inbox from the backend. Assert every Data key and the enum→
// string conversion.
func TestInboxEventForMapping(t *testing.T) {
	delivered := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	msg := QueuedUserMessage{
		ID:          "m1",
		Text:        "hello",
		Status:      QueuedMessageStatusDelivered,
		QueuedAt:    time.Date(2026, 6, 23, 11, 0, 0, 0, time.UTC),
		DeliveredAt: &delivered,
	}

	evt := InboxEventFor(EventUserMessageDelivered, "run-inbox-evt", msg)

	if evt.Type != EventUserMessageDelivered {
		t.Errorf("Type = %q, want %q", evt.Type, EventUserMessageDelivered)
	}
	if evt.RunID != "run-inbox-evt" {
		t.Errorf("RunID = %q, want run-inbox-evt", evt.RunID)
	}
	if evt.Data["id"] != "m1" {
		t.Errorf("Data[id] = %v, want m1", evt.Data["id"])
	}
	if evt.Data["text"] != "hello" {
		t.Errorf("Data[text] = %v, want hello", evt.Data["text"])
	}
	// Status must be the string form, not the typed enum value.
	if evt.Data["status"] != "delivered" {
		t.Errorf("Data[status] = %v (%T), want string \"delivered\"", evt.Data["status"], evt.Data["status"])
	}
	if evt.Data["queued_at"] != msg.QueuedAt {
		t.Errorf("Data[queued_at] = %v, want %v", evt.Data["queued_at"], msg.QueuedAt)
	}
	gotDelivered, ok := evt.Data["delivered_at"].(*time.Time)
	if !ok || gotDelivered == nil || !gotDelivered.Equal(delivered) {
		t.Errorf("Data[delivered_at] = %v, want %v", evt.Data["delivered_at"], delivered)
	}
	// Unset transition timestamps must still be present as keys so
	// consumers can distinguish "absent value" from "missing field".
	if _, present := evt.Data["consumed_at"]; !present {
		t.Error("Data missing consumed_at key")
	}
	if _, present := evt.Data["cancelled_at"]; !present {
		t.Error("Data missing cancelled_at key")
	}
}

// PublishInboxEvent must both persist the event (Seq-stamped by the
// store) AND fan it out to the live subscriber exactly once.
func TestPublishInboxEventPersistsAndPublishes(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-publish"

	var published []Event
	publish := func(e Event) { published = append(published, e) }

	msg := QueuedUserMessage{ID: "m1", Text: "hi", Status: QueuedMessageStatusQueued}
	PublishInboxEvent(ctx, s, publish, EventUserMessageQueued, runID, msg)

	if len(published) != 1 {
		t.Fatalf("publish called %d times, want 1", len(published))
	}
	if published[0].Type != EventUserMessageQueued {
		t.Errorf("published Type = %q, want %q", published[0].Type, EventUserMessageQueued)
	}
	if published[0].Data["id"] != "m1" {
		t.Errorf("published Data[id] = %v, want m1", published[0].Data["id"])
	}

	// The event must also be on disk.
	evts, err := s.LoadEvents(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("persisted %d events, want 1", len(evts))
	}
	if evts[0].Type != EventUserMessageQueued || evts[0].Data["id"] != "m1" {
		t.Errorf("persisted event = %+v, want queued event for m1", evts[0])
	}
}

func TestPublishInboxEventNilPublishSafe(t *testing.T) {
	s := tmpStore(t)
	ctx := context.Background()
	const runID = "run-publish-nil"

	msg := QueuedUserMessage{ID: "m2", Text: "yo", Status: QueuedMessageStatusQueued}
	// Must not panic with a nil publish callback (cloud mode passes nil).
	PublishInboxEvent(ctx, s, nil, EventUserMessageQueued, runID, msg)

	evts, err := s.LoadEvents(ctx, runID)
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(evts) != 1 {
		t.Fatalf("persisted %d events, want 1 (event must still be written)", len(evts))
	}
}
