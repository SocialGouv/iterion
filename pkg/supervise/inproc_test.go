package supervise

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/store"
)

func TestEventHubFanOut(t *testing.T) {
	h := NewEventHub()
	ch1, rel1, err := h.ObserveRun(context.Background(), "ignored")
	if err != nil {
		t.Fatal(err)
	}
	defer rel1()
	ch2, rel2, err := h.ObserveRun(context.Background(), "ignored")
	if err != nil {
		t.Fatal(err)
	}
	defer rel2()

	h.Publish(store.Event{Seq: 1, Type: store.EventNodeStarted, NodeID: "a"})

	e1, e2 := <-ch1, <-ch2
	if e1 == nil || e2 == nil || e1.Seq != 1 || e2.Seq != 1 {
		t.Fatalf("fan-out = %+v / %+v", e1, e2)
	}
	// Each subscriber gets its own copy — mutating one must not leak.
	if e1 == e2 {
		t.Error("subscribers received the same *Event pointer")
	}
	e1.NodeID = "mutated"
	if e2.NodeID != "a" {
		t.Error("mutation leaked across subscribers")
	}
}

// A subscriber that never reads drops events instead of blocking the
// publisher.
func TestEventHubNonBlockingDrop(t *testing.T) {
	h := NewEventHub()
	ch, release, err := h.ObserveRun(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	for i := 0; i < subscriberBufferSize+10; i++ {
		h.Publish(store.Event{Seq: int64(i)}) // must not block
	}
	release()
	var n int
	for range ch {
		n++
	}
	if n != subscriberBufferSize {
		t.Errorf("buffered events = %d; want %d (overflow dropped)", n, subscriberBufferSize)
	}
}

func TestEventHubReleaseIdempotentAndSafe(t *testing.T) {
	h := NewEventHub()
	ch, release, err := h.ObserveRun(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	release()
	release() // second call must not panic (close-once)
	if _, ok := <-ch; ok {
		t.Error("channel not closed after release")
	}
	// Publishing after release must not panic (subscriber deregistered).
	h.Publish(store.Event{Seq: 9})
}

func TestStoreInjectorAppendsNodeScopedMessage(t *testing.T) {
	ctx := context.Background()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	inj := &StoreInjector{Store: st}

	if err := inj.Inject(ctx, "r1", "implement", "check the diff"); err != nil {
		t.Fatalf("Inject: %v", err)
	}

	msgs, err := st.LoadPendingQueuedMessages(ctx, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("pending = %d; want 1", len(msgs))
	}
	if msgs[0].NodeID != "implement" || msgs[0].Text != "check the diff" {
		t.Errorf("msg = %+v; want node-scoped text", msgs[0])
	}
	if !strings.HasPrefix(msgs[0].ID, "msg_") {
		t.Errorf("id = %q; want msg_ prefix", msgs[0].ID)
	}

	// The node-scoped drain semantics hold: a different active node
	// leaves it queued, the tagged node drains it.
	texts, _, err := store.DrainPendingForNode(ctx, st, nil, "r1", "other")
	if err != nil || len(texts) != 0 {
		t.Fatalf("drain for other node = (%v, %v); want no delivery", texts, err)
	}
	texts, _, err = store.DrainPendingForNode(ctx, st, nil, "r1", "implement")
	if err != nil || len(texts) != 1 || texts[0] != "check the diff" {
		t.Fatalf("drain for implement = (%v, %v); want the message", texts, err)
	}
}

func TestInboxRunID(t *testing.T) {
	if got := inboxRunID(""); got != inboxSessionKey {
		t.Errorf("inboxRunID(\"\") = %q; want %q", got, inboxSessionKey)
	}
	if got := inboxRunID("sess-9"); got != "sess-9" {
		t.Errorf("inboxRunID(sess-9) = %q", got)
	}
}

func TestNewInboxMessageID(t *testing.T) {
	a, b := newInboxMessageID(), newInboxMessageID()
	if !strings.HasPrefix(a, "msg_") || !strings.HasPrefix(b, "msg_") {
		t.Errorf("ids = %q, %q; want msg_ prefix", a, b)
	}
	if a == b {
		t.Errorf("two ids collided: %q", a)
	}
}

// Two sessions of the same project have independent inboxes.
func TestInboxInjectorSessionIsolation(t *testing.T) {
	t.Setenv("ITERION_HOME", t.TempDir())
	ctx := context.Background()
	const key = "-home-x-proj"

	injA, err := NewInboxInjector(key, "sess-a")
	if err != nil {
		t.Fatal(err)
	}
	injB, err := NewInboxInjector(key, "sess-b")
	if err != nil {
		t.Fatal(err)
	}
	if err := injA.Inject(ctx, "", "", "for A"); err != nil {
		t.Fatal(err)
	}
	if err := injB.Inject(ctx, "", "", "for B"); err != nil {
		t.Fatal(err)
	}

	gotA, err := DrainClaudeInbox(ctx, key, "sess-a")
	if err != nil || len(gotA) != 1 || gotA[0] != "for A" {
		t.Fatalf("drain A = (%v, %v)", gotA, err)
	}
	gotB, err := DrainClaudeInbox(ctx, key, "sess-b")
	if err != nil || len(gotB) != 1 || gotB[0] != "for B" {
		t.Fatalf("drain B = (%v, %v)", gotB, err)
	}
}
