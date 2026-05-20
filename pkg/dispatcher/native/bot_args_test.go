package native

import (
	"testing"
)

// TestCreate_PersistsBotAndArgs checks the new ticket-level fields
// survive a write + read round trip.
func TestCreate_PersistsBotAndArgs(t *testing.T) {
	s := newTestStore(t)
	iss, err := s.Create(Issue{
		Title: "ship feature X",
		State: "ready",
		Bot:   "feature_dev",
		BotArgs: map[string]string{
			"workspace_dir": "/tmp/x",
			"loop_cap":      "5",
			"dry_run":       "false",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := s.Get(iss.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Bot != "feature_dev" {
		t.Errorf("Bot = %q", got.Bot)
	}
	if len(got.BotArgs) != 3 {
		t.Fatalf("BotArgs = %v", got.BotArgs)
	}
	if got.BotArgs["loop_cap"] != "5" {
		t.Errorf("BotArgs[loop_cap] = %q", got.BotArgs["loop_cap"])
	}
}

// TestUpdate_SetsAndClearsBot exercises both directions: assigning a
// bot and then clearing it back to dispatcher-default.
func TestUpdate_SetsAndClearsBot(t *testing.T) {
	s := newTestStore(t)
	iss, _ := s.Create(Issue{Title: "x", State: "ready"})

	bot := "feature_dev"
	updated, err := s.Update(iss.ID, Patch{Bot: &bot})
	if err != nil {
		t.Fatalf("Update set: %v", err)
	}
	if updated.Bot != "feature_dev" {
		t.Errorf("set: Bot = %q", updated.Bot)
	}

	empty := ""
	cleared, err := s.Update(iss.ID, Patch{Bot: &empty})
	if err != nil {
		t.Fatalf("Update clear: %v", err)
	}
	if cleared.Bot != "" {
		t.Errorf("clear: Bot = %q", cleared.Bot)
	}
}

// TestUpdate_SwapsBotArgsAtomically — the studio always sends the
// whole form, so a patch BotArgs replaces the stored map wholesale
// (no merging per-key like Fields).
func TestUpdate_SwapsBotArgsAtomically(t *testing.T) {
	s := newTestStore(t)
	iss, _ := s.Create(Issue{
		Title:   "x",
		State:   "ready",
		BotArgs: map[string]string{"a": "1", "b": "2"},
	})

	next := map[string]string{"a": "9", "c": "3"}
	updated, err := s.Update(iss.ID, Patch{BotArgs: &next})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(updated.BotArgs) != 2 {
		t.Fatalf("expected 2 keys after swap, got %v", updated.BotArgs)
	}
	if updated.BotArgs["a"] != "9" {
		t.Errorf("a = %q", updated.BotArgs["a"])
	}
	if _, ok := updated.BotArgs["b"]; ok {
		t.Errorf("b should have been dropped: %v", updated.BotArgs)
	}
	if updated.BotArgs["c"] != "3" {
		t.Errorf("c = %q", updated.BotArgs["c"])
	}
}

// TestUpdate_EmptyBotArgsClears — passing an empty (but non-nil) map
// wipes the field entirely. This is the "user cleared the form"
// signal from the studio.
func TestUpdate_EmptyBotArgsClears(t *testing.T) {
	s := newTestStore(t)
	iss, _ := s.Create(Issue{
		Title:   "x",
		State:   "ready",
		BotArgs: map[string]string{"a": "1"},
	})
	empty := map[string]string{}
	updated, err := s.Update(iss.ID, Patch{BotArgs: &empty})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if len(updated.BotArgs) != 0 {
		t.Errorf("BotArgs not cleared: %v", updated.BotArgs)
	}
}

// TestAdapter_PropagatesBot confirms toTrackerIssue carries the new
// fields across the dispatcher boundary.
func TestAdapter_PropagatesBot(t *testing.T) {
	s := newTestStore(t)
	iss, _ := s.Create(Issue{
		Title:   "x",
		State:   "ready",
		Bot:     "feature_dev",
		BotArgs: map[string]string{"k": "v"},
	})
	got := toTrackerIssue(iss)
	if got.Bot != "feature_dev" {
		t.Errorf("tracker.Bot = %q", got.Bot)
	}
	if got.BotArgs["k"] != "v" {
		t.Errorf("tracker.BotArgs[k] = %q", got.BotArgs["k"])
	}
	// Mutating the adapter's clone must not bleed back into the store.
	got.BotArgs["k"] = "mutated"
	again, _ := s.Get(iss.ID)
	if again.BotArgs["k"] != "v" {
		t.Errorf("store mutation leak: %v", again.BotArgs)
	}
}
