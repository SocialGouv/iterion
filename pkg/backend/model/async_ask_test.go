package model

import (
	"context"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/backend/delegate"
	"github.com/SocialGouv/iterion/pkg/store"
)

func asyncBinderFixture(t *testing.T) (*StoreAsyncAskBinder, store.RunStore, []store.Event) {
	t.Helper()
	s, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	var published []store.Event
	b := &StoreAsyncAskBinder{
		Store:   s,
		Publish: func(e store.Event) { published = append(published, e) },
	}
	return b, s, published
}

func TestStoreAsyncAskBinder_PostPendingCollect(t *testing.T) {
	b, s, _ := asyncBinderFixture(t)
	ctx := context.Background()
	hook := b.BindAsyncAsk(ctx, "r1", "asker")
	if hook == nil {
		t.Fatal("hook is nil")
	}

	// Post two questions → sequential ids, pending in order.
	id1, err := hook.Post(ctx, delegate.AsyncQuestion{Question: "color?"})
	if err != nil {
		t.Fatalf("post 1: %v", err)
	}
	if id1 != "r1_asker_async_1" {
		t.Errorf("id1 = %s, want r1_asker_async_1", id1)
	}
	id2, err := hook.Post(ctx, delegate.AsyncQuestion{
		Question:      "size?",
		Options:       []delegate.AskUserOption{{ID: "s", Label: "Small"}, {ID: "l", Label: "Large"}},
		AllowFreeText: true,
	})
	if err != nil {
		t.Fatalf("post 2: %v", err)
	}
	if id2 != "r1_asker_async_2" {
		t.Errorf("id2 = %s, want r1_asker_async_2", id2)
	}

	// Empty question is an explicit error, never a silent no-op.
	if _, err := hook.Post(ctx, delegate.AsyncQuestion{Question: "  "}); err == nil {
		t.Error("empty question: want error")
	}

	pending, err := hook.Pending(ctx)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 2 || pending[0].InteractionID != id1 || pending[0].Question != "color?" {
		t.Fatalf("pending = %+v, want [color?, size?]", pending)
	}

	// human_input_requested{async:true} persisted per post.
	events, err := s.LoadEvents(ctx, "r1")
	if err != nil {
		t.Fatalf("load events: %v", err)
	}
	asked := 0
	for _, e := range events {
		if e.Type == store.EventHumanInputRequested && e.Data["async"] == true {
			asked++
		}
	}
	if asked != 2 {
		t.Errorf("async human_input_requested events = %d, want 2", asked)
	}

	// Answer both → Pending empties, CollectAnswers formats both Q/A.
	if _, err := store.AnswerInteraction(ctx, s, "r1", id1, map[string]any{delegate.AskUserQuestionKey: "blue"}); err != nil {
		t.Fatalf("answer 1: %v", err)
	}
	if _, err := store.AnswerInteraction(ctx, s, "r1", id2, map[string]any{delegate.AskUserQuestionKey: "l"}); err != nil {
		t.Fatalf("answer 2: %v", err)
	}
	pending, err = hook.Pending(ctx)
	if err != nil {
		t.Fatalf("pending after answers: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after answers = %d, want 0", len(pending))
	}
	text, err := hook.CollectAnswers(ctx)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, want := range []string{id1, "color?", "blue", id2, "size?", "l"} {
		if !strings.Contains(text, want) {
			t.Errorf("collected answers missing %q:\n%s", want, text)
		}
	}
}

func TestStoreAsyncAskBinder_NilPaths(t *testing.T) {
	var b *StoreAsyncAskBinder
	if hook := b.BindAsyncAsk(context.Background(), "r1", "n1"); hook != nil {
		t.Error("nil binder must bind nil hook")
	}
	b2 := &StoreAsyncAskBinder{}
	if hook := b2.BindAsyncAsk(context.Background(), "r1", "n1"); hook != nil {
		t.Error("store-less binder must bind nil hook")
	}
	b3, _, _ := asyncBinderFixture(t)
	if hook := b3.BindAsyncAsk(context.Background(), "", "n1"); hook != nil {
		t.Error("empty runID must bind nil hook")
	}
	if hook := b3.BindAsyncAsk(context.Background(), "r1", ""); hook != nil {
		t.Error("empty nodeID must bind nil hook")
	}
}

func TestCollectAsyncAnswersText_NothingPosted(t *testing.T) {
	b, _, _ := asyncBinderFixture(t)
	hook := b.BindAsyncAsk(context.Background(), "r1", "asker")
	text, err := hook.CollectAnswers(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !strings.Contains(text, "No async questions were posted") {
		t.Errorf("empty-collect text = %q", text)
	}
}

func TestFormatAsyncAnswerMessage(t *testing.T) {
	got := FormatAsyncAnswerMessage("r1_asker_async_1", "color?", "blue")
	for _, want := range []string{"r1_asker_async_1", "color?", "blue", "[Answer to question"} {
		if !strings.Contains(got, want) {
			t.Errorf("formatted message missing %q: %s", want, got)
		}
	}
}
