package model

import (
	"context"
	"errors"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	clawrt "github.com/SocialGouv/claw-code-go/pkg/runtime"
)

func TestNodeSessionStore_LoadSaveEvict(t *testing.T) {
	s := newNodeSessionStore()

	// Empty store returns nil.
	if got := s.load("run", "n1"); got != nil {
		t.Fatalf("empty load = %v, want nil", got)
	}

	msgs := []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "hello"}}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: "world"}}},
	}
	s.save("run", "n1", msgs)

	got := s.load("run", "n1")
	if len(got) != 2 {
		t.Fatalf("loaded %d messages, want 2", len(got))
	}

	// Defensive copy: mutating the loaded slice does not affect the store.
	got[0].Role = "tampered"
	got2 := s.load("run", "n1")
	if got2[0].Role != "user" {
		t.Fatalf("store leaked underlying slice: got %q after mutation", got2[0].Role)
	}

	// Different (run, node) buckets are isolated.
	s.save("run", "n2", msgs[:1])
	if got := s.load("run", "n1"); len(got) != 2 {
		t.Fatalf("n1 mutated by n2 save: %d msgs", len(got))
	}

	s.evict("run", "n1")
	if got := s.load("run", "n1"); got != nil {
		t.Fatalf("post-evict load = %v, want nil", got)
	}
	if got := s.load("run", "n2"); len(got) != 1 {
		t.Fatalf("evict spilled across nodes: n2 has %d msgs", len(got))
	}

	// Save with empty slice = evict.
	s.save("run", "n2", nil)
	if got := s.load("run", "n2"); got != nil {
		t.Fatalf("save(nil) did not evict: got %v", got)
	}
}

func TestNodeSessionStore_CompactNoOpWhenSmall(t *testing.T) {
	s := newNodeSessionStore()
	// 3 short messages — well under MaxEstimatedTokens=10000.
	msgs := []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "a"}}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: "b"}}},
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "c"}}},
	}
	s.save("run", "node", msgs)

	removed, fired := s.compact("run", "node", clawrt.DefaultCompactionConfig())
	if fired {
		t.Fatalf("compact fired on tiny session (removed=%d)", removed)
	}
	if got := s.load("run", "node"); len(got) != 3 {
		t.Fatalf("compact mutated session despite no-op: got %d msgs", len(got))
	}
}

func TestNodeSessionStore_CompactRunsOnLargeSession(t *testing.T) {
	s := newNodeSessionStore()
	// Build a session that exceeds the compactor's heuristic so it
	// actually fires. Each message carries enough text to push the
	// estimate past MaxEstimatedTokens.
	msgs := make([]api.Message, 0, 20)
	for i := 0; i < 20; i++ {
		msgs = append(msgs, api.Message{
			Role: "user",
			Content: []api.ContentBlock{{
				Type: "text",
				Text: largePadding(),
			}},
		})
	}
	s.save("run", "node", msgs)

	removed, fired := s.compact("run", "node", clawrt.DefaultCompactionConfig())
	if !fired {
		t.Fatalf("expected compact to fire on a 20-message large-text session")
	}
	if removed <= 0 {
		t.Fatalf("compact fired but removed=%d", removed)
	}
	got := s.load("run", "node")
	if len(got) >= 20 {
		t.Fatalf("post-compact session size = %d (was 20); compactor did not shrink it", len(got))
	}
}

func TestRunIDContext_RoundTrip(t *testing.T) {
	ctx := context.Background()
	if got := RunIDFromContext(ctx); got != "" {
		t.Fatalf("empty ctx returned runID %q", got)
	}
	ctx = WithRunID(ctx, "abc")
	if got := RunIDFromContext(ctx); got != "abc" {
		t.Fatalf("RunIDFromContext = %q, want abc", got)
	}
	// Empty input is a no-op (does not overwrite).
	ctx2 := WithRunID(ctx, "")
	if got := RunIDFromContext(ctx2); got != "abc" {
		t.Fatalf("WithRunID(\"\") overwrote runID: got %q", got)
	}
}

func TestApplyAndCaptureSession(t *testing.T) {
	store := newNodeSessionStore()
	ctx := withRuntimeContext(context.Background(), "run-1", store)

	// First call: no prior session, opts.Messages unchanged.
	opts := GenerationOptions{
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "first"}}},
		},
	}
	got := applySessionMessages(ctx, "node-1", opts)
	if len(got.Messages) != 1 {
		t.Fatalf("applySessionMessages prepended without a session: got %d msgs", len(got.Messages))
	}

	// Capture an end-of-attempt result with two assistant turns.
	captureSessionMessages(ctx, "node-1", &TextResult{
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "first"}}},
			{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: "halfway answer"}}},
		},
	})

	// Second call: prior messages prepended, original 1 message preserved.
	opts2 := GenerationOptions{
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "retry-prompt"}}},
		},
	}
	got2 := applySessionMessages(ctx, "node-1", opts2)
	if len(got2.Messages) != 3 {
		t.Fatalf("retry messages = %d, want 3 (2 prior + 1 new)", len(got2.Messages))
	}
	if got2.Messages[0].Content[0].Text != "first" {
		t.Fatalf("retry order broken: first message text = %q", got2.Messages[0].Content[0].Text)
	}

	// nil result is a no-op (does not clobber the stored session).
	captureSessionMessages(ctx, "node-1", nil)
	stored := store.load("run-1", "node-1")
	if len(stored) != 2 {
		t.Fatalf("nil result wiped the session: %d msgs left", len(stored))
	}
}

func TestClawExecutorCompact_AppliesToStoredSession(t *testing.T) {
	e := &ClawExecutor{sessions: newNodeSessionStore()}

	// Build a large session and stash it as if a prior attempt had run.
	msgs := make([]api.Message, 0, 20)
	for i := 0; i < 20; i++ {
		msgs = append(msgs, api.Message{
			Role: "user",
			Content: []api.ContentBlock{{
				Type: "text",
				Text: largePadding(),
			}},
		})
	}
	e.sessions.save("run-1", "node-1", msgs)

	ctx := WithRunID(context.Background(), "run-1")

	if err := e.Compact(ctx, "node-1"); err != nil {
		t.Fatalf("Compact returned error: %v", err)
	}
	got := e.sessions.load("run-1", "node-1")
	if len(got) >= 20 {
		t.Fatalf("session not reduced: %d msgs", len(got))
	}
}

func TestClawExecutorCompact_NoSessionReturnsErrCompactionUnsupported(t *testing.T) {
	e := &ClawExecutor{sessions: newNodeSessionStore()}
	ctx := WithRunID(context.Background(), "run-1")
	err := e.Compact(ctx, "node-without-session")
	if err == nil {
		t.Fatalf("Compact on missing session returned nil")
	}
	if !errors.Is(err, ErrCompactionUnsupported) {
		t.Fatalf("Compact error chain missing ErrCompactionUnsupported: %v", err)
	}
}

func TestClawExecutorCompact_NoRunIDReturnsErrCompactionUnsupported(t *testing.T) {
	e := &ClawExecutor{sessions: newNodeSessionStore()}
	err := e.Compact(context.Background(), "node-1")
	if !errors.Is(err, ErrCompactionUnsupported) {
		t.Fatalf("Compact without runID should return ErrCompactionUnsupported, got %v", err)
	}
}

// largePadding returns a string of plausible per-message text large
// enough that 20 of them push the heuristic compactor past its
// default threshold of 10 000 estimated tokens.
func largePadding() string {
	const para = "lorem ipsum dolor sit amet consectetur adipiscing elit sed do eiusmod tempor incididunt ut labore et dolore magna aliqua "
	out := ""
	for i := 0; i < 20; i++ {
		out += para
	}
	return out
}
