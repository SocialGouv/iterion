package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func asyncStoreFixture(t *testing.T) RunStore {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	return s
}

func writeAsync(t *testing.T, s RunStore, runID, nodeID, id string, answered bool, at time.Time) {
	t.Helper()
	in := &Interaction{
		ID:          id,
		RunID:       runID,
		NodeID:      nodeID,
		Kind:        InteractionKindAsync,
		RequestedAt: at,
		Questions:   map[string]any{"ask_user_response": "q-" + id},
	}
	if answered {
		now := at.Add(time.Minute)
		in.AnsweredAt = &now
		in.Answers = map[string]any{"ask_user_response": "a-" + id}
	}
	if err := s.WriteInteraction(context.Background(), in); err != nil {
		t.Fatalf("write interaction %s: %v", id, err)
	}
}

func TestListPendingAsyncInteractions(t *testing.T) {
	s := asyncStoreFixture(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)

	writeAsync(t, s, "r1", "asker", "r1_asker_async_2", false, base.Add(time.Second))
	writeAsync(t, s, "r1", "asker", "r1_asker_async_1", false, base)
	writeAsync(t, s, "r1", "asker", "r1_asker_async_3", true, base) // answered → excluded
	writeAsync(t, s, "r1", "other", "r1_other_async_1", false, base)
	// A blocking (kind "") interaction is never listed as async.
	if err := s.WriteInteraction(ctx, &Interaction{ID: "r1_pause", RunID: "r1", NodeID: "asker", RequestedAt: base}); err != nil {
		t.Fatalf("write blocking interaction: %v", err)
	}

	all, err := ListPendingAsyncInteractions(ctx, s, "r1", "")
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("all pending = %d, want 3", len(all))
	}
	if all[0].ID != "r1_asker_async_1" {
		t.Errorf("oldest-first ordering broken: first = %s", all[0].ID)
	}

	scoped, err := ListPendingAsyncInteractions(ctx, s, "r1", "asker")
	if err != nil {
		t.Fatalf("list scoped: %v", err)
	}
	if len(scoped) != 2 {
		t.Fatalf("asker pending = %d, want 2", len(scoped))
	}
}

func TestAnswerInteraction_RefusesDoubleAnswer(t *testing.T) {
	s := asyncStoreFixture(t)
	ctx := context.Background()
	writeAsync(t, s, "r1", "asker", "r1_asker_async_1", false, time.Now().UTC())

	in, err := AnswerInteraction(ctx, s, "r1", "r1_asker_async_1", map[string]any{"ask_user_response": "yes"})
	if err != nil {
		t.Fatalf("first answer: %v", err)
	}
	if in.AnsweredAt == nil || in.Answers["ask_user_response"] != "yes" {
		t.Fatalf("answer not recorded: %+v", in)
	}

	if _, err := AnswerInteraction(ctx, s, "r1", "r1_asker_async_1", map[string]any{"ask_user_response": "no"}); !errors.Is(err, ErrInteractionAlreadyAnswered) {
		t.Fatalf("second answer error = %v, want ErrInteractionAlreadyAnswered", err)
	}
	// The first answer must survive.
	reloaded, err := s.LoadInteraction(ctx, "r1", "r1_asker_async_1")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Answers["ask_user_response"] != "yes" {
		t.Errorf("first answer overwritten: %v", reloaded.Answers)
	}
}
