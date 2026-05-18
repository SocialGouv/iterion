package model

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/SocialGouv/iterion/pkg/store"
)

// TestStoreInboxBinder_DrainAndConsume verifies that:
//   - Bind() returns a working drain/consume pair when run+store are valid
//   - drain returns texts in FIFO order and transitions status to delivered
//   - consume only transitions previously-drained messages to consumed
//   - a second drain immediately after returns empty (no re-delivery)
//   - delivered + consumed events are published in lockstep
func TestStoreInboxBinder_DrainAndConsume(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.New(dir)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	ctx := context.Background()
	const runID = "run_inbox"
	if _, err := s.CreateRun(ctx, runID, "demo", nil); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	now := time.Now().UTC()
	msgs := []store.QueuedUserMessage{
		{ID: "m1", Text: "ping", QueuedAt: now.Add(0)},
		{ID: "m2", Text: "context", QueuedAt: now.Add(time.Millisecond)},
	}
	for _, m := range msgs {
		if err := s.AppendQueuedMessage(ctx, runID, m); err != nil {
			t.Fatalf("AppendQueuedMessage: %v", err)
		}
	}

	var published []store.Event
	binder := &StoreInboxBinder{
		Store: s,
		Publish: func(e store.Event) {
			published = append(published, e)
		},
	}
	hook := binder.Bind(ctx, runID)
	if hook == nil {
		t.Fatal("Bind returned nil hook for a configured binder")
	}

	// 1) First drain returns both messages in FIFO order and marks
	//    them delivered in-store.
	got := hook.Drain(ctx)
	if len(got) != 2 || got[0] != "ping" || got[1] != "context" {
		t.Fatalf("drain returned %v, want [ping, context]", got)
	}
	pending, err := s.LoadPendingQueuedMessages(ctx, runID)
	if err != nil {
		t.Fatalf("LoadPending: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("post-drain pending = %d, want 0", len(pending))
	}
	if got := countEvents(published, store.EventUserMessageDelivered); got != 2 {
		t.Fatalf("delivered events = %d, want 2", got)
	}

	// 2) A second drain immediately after returns nothing — delivered
	//    messages are not redelivered.
	if got := hook.Drain(ctx); len(got) != 0 {
		t.Fatalf("second drain returned %v, want empty", got)
	}

	// 3) consume transitions the previously-drained messages to
	//    consumed and emits the matching events. Reset published so
	//    we only count the new transitions.
	published = nil
	hook.Consume(ctx)
	if got := countEvents(published, store.EventUserMessageConsumed); got != 2 {
		t.Fatalf("consumed events = %d, want 2", got)
	}
	all, err := s.ListQueuedMessages(ctx, runID)
	if err != nil {
		t.Fatalf("ListQueued: %v", err)
	}
	for _, m := range all {
		if m.Status != store.QueuedMessageStatusConsumed {
			t.Errorf("status for %s = %s, want consumed", m.ID, m.Status)
		}
	}

	// 4) A consume with no fresh delivery is a no-op (idempotent).
	published = nil
	hook.Consume(ctx)
	if len(published) != 0 {
		t.Fatalf("idempotent consume published %d events, want 0", len(published))
	}
}

// TestStoreInboxBinder_NilSafety verifies the binder degrades to
// (nil, nil) on missing config so the backend's generation loop runs
// unaffected.
func TestStoreInboxBinder_NilSafety(t *testing.T) {
	t.Helper()
	t.Run("nil receiver", func(t *testing.T) {
		var b *StoreInboxBinder
		if b.Bind(context.Background(), "run_x") != nil {
			t.Fatal("nil receiver should disable inbox")
		}
	})
	t.Run("missing run ID", func(t *testing.T) {
		dir := t.TempDir()
		s, err := store.New(dir)
		if err != nil {
			t.Fatalf("store.New: %v", err)
		}
		b := &StoreInboxBinder{Store: s}
		if b.Bind(context.Background(), "") != nil {
			t.Fatal("empty run ID should disable inbox")
		}
	})
}

// TestBuildOperatorMessage_FormatsBlock verifies the synthetic user
// turn carries the [OPERATOR MESSAGE] prefix and concatenates
// multiple texts with a separator so the LLM can distinguish them.
func TestBuildOperatorMessage_FormatsBlock(t *testing.T) {
	t.Helper()
	msg := buildOperatorMessage([]string{"first", "second"})
	if msg.Role != "user" {
		t.Errorf("role = %q, want user", msg.Role)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "text" {
		t.Fatalf("content = %+v, want single text block", msg.Content)
	}
	text := msg.Content[0].Text
	if !strings.HasPrefix(text, "[OPERATOR MESSAGE]") {
		t.Errorf("text does not start with marker: %q", text)
	}
	if !strings.Contains(text, "first") || !strings.Contains(text, "second") {
		t.Errorf("text missing original messages: %q", text)
	}
}

func countEvents(events []store.Event, typ store.EventType) int {
	n := 0
	for _, e := range events {
		if e.Type == typ {
			n++
		}
	}
	return n
}
