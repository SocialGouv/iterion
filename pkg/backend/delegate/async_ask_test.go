package delegate

import (
	"encoding/json"
	"strings"
	"testing"
)

// AwaitPending round-trip: the []PendingAsync refs survive the
// Questions-map → checkpoint JSON → resume parse cycle (everything
// degrades to []any / map[string]any across persistence).
func TestAwaitPendingRoundTrip(t *testing.T) {
	refs := []PendingAsync{
		{InteractionID: "r1_a_async_1", Question: "color?"},
		{InteractionID: "r1_a_async_2", Question: "size?"},
	}
	questions := map[string]any{AwaitPendingInteractionsKey: AwaitPendingToQuestions(refs)}

	// Simulate checkpoint persistence.
	raw, err := json.Marshal(questions)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var revived map[string]any
	if err := json.Unmarshal(raw, &revived); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	got := ParseAwaitPending(revived[AwaitPendingInteractionsKey])
	if len(got) != 2 {
		t.Fatalf("parsed %d refs, want 2", len(got))
	}
	if got[0] != refs[0] || got[1] != refs[1] {
		t.Errorf("round-trip mismatch: %+v != %+v", got, refs)
	}
}

func TestParseAwaitPending_Tolerant(t *testing.T) {
	if got := ParseAwaitPending(nil); got != nil {
		t.Errorf("nil input → %v, want nil", got)
	}
	if got := ParseAwaitPending("garbage"); got != nil {
		t.Errorf("non-list input → %v, want nil", got)
	}
	// Entries without an interaction_id are dropped, not fatal.
	got := ParseAwaitPending([]any{
		map[string]any{"question": "orphan"},
		map[string]any{"interaction_id": "ok", "question": "kept"},
		"not-a-map",
	})
	if len(got) != 1 || got[0].InteractionID != "ok" {
		t.Errorf("tolerant parse = %+v, want single 'ok' ref", got)
	}
}

// The async protocol section rides the system prompt ONLY when the
// executor bound PostAsyncQuestion (interaction: async).
func TestBuildSystemPrompt_AsyncSection(t *testing.T) {
	base := Task{SystemPrompt: "do things", InteractionEnabled: true}
	if got := base.BuildSystemPrompt(); strings.Contains(got, "[ASYNC QUESTIONS]") {
		t.Error("async section present without PostAsyncQuestion binding")
	}

	async := base
	async.PostAsyncQuestion = func(AsyncQuestion) (string, error) { return "", nil }
	got := async.BuildSystemPrompt()
	if !strings.Contains(got, "[ASYNC QUESTIONS]") {
		t.Error("async section missing on an interaction: async task")
	}
	if !strings.Contains(got, "[INTERACTION PROTOCOL]") {
		t.Error("base interaction protocol must still precede the async section")
	}
	if !strings.Contains(got, AskUserAsyncToolName) || !strings.Contains(got, AwaitAnswersToolName) {
		t.Error("async section must name both tools")
	}
}
