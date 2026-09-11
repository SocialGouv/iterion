package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
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

func TestClawPersistEnvelope_RoundTripNamedSlot(t *testing.T) {
	source := &ClawExecutor{sessions: newNodeSessionStore()}
	messages := []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "remember correction A"}}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: "correction A recorded"}}},
	}
	source.sessions.save("run-1", "assistant_conversation", messages)
	blob := source.packClawSession("run-1", "assistant_conversation", "session-1", "claw:openai")
	if len(blob) == 0 || len(blob) > clawPersistHardBlobBytes {
		t.Fatalf("packed blob length=%d", len(blob))
	}
	target := &ClawExecutor{sessions: newNodeSessionStore()}
	ctx := WithRunID(context.Background(), "run-1")
	if err := target.UnpackSession(ctx, "claw", "session-1", blob); err != nil {
		t.Fatalf("unpack: %v", err)
	}
	got := target.sessions.load("run-1", "assistant_conversation")
	if len(got) != len(messages) || got[0].Content[0].Text != "remember correction A" {
		t.Fatalf("round-trip messages=%#v", got)
	}
}

func TestClawPersistEnvelope_CompactsLargeToolResultsForReplay(t *testing.T) {
	const runID, slot, sessionID = "run-large-tool-results", "assistant_conversation", "session-large-tool-results"
	largeResult := strings.Repeat("tool output that token estimation must not replay verbatim\n", 1_300)
	messages := []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "inspect the current workflow"}}},
		toolUseMessage("large-1"),
		{Role: "user", Content: []api.ContentBlock{api.ToolResult{ToolUseID: "large-1", Content: largeResult}.ToContentBlock()}},
		{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: "continue with the evidence"}}},
		toolUseMessage("large-2"),
		{Role: "user", Content: []api.ContentBlock{api.ToolResult{ToolUseID: "large-2", Content: largeResult}.ToContentBlock()}},
		toolUseMessage("large-3"),
		{Role: "user", Content: []api.ContentBlock{api.ToolResult{ToolUseID: "large-3", Content: largeResult}.ToContentBlock()}},
	}
	if _, compacted := forceCompactToTokens(messages, clawPersistTargetTokens, clawPersistRecent); compacted {
		t.Fatal("precondition: token-only compaction unexpectedly handled nested tool results")
	}

	source := &ClawExecutor{sessions: newNodeSessionStore()}
	source.sessions.save(runID, slot, messages)
	legacyBlob, err := marshalClawPersistEnvelope(slot, sessionID, "claw:openai", messages)
	if err != nil {
		t.Fatalf("marshal legacy envelope: %v", err)
	}
	if len(legacyBlob) <= clawPersistReplayBlobBytes || len(legacyBlob) > clawPersistHardBlobBytes {
		t.Fatalf("legacy envelope length=%d, want %d..%d", len(legacyBlob), clawPersistReplayBlobBytes+1, clawPersistHardBlobBytes)
	}
	blob := source.packClawSession(runID, slot, sessionID, "claw:openai")
	if len(blob) == 0 || len(blob) > clawPersistReplayBlobBytes {
		t.Fatalf("packed replay length=%d, want 1..%d", len(blob), clawPersistReplayBlobBytes)
	}
	var envelope clawPersistEnvelope
	if err := json.Unmarshal(blob, &envelope); err != nil {
		t.Fatalf("decode packed envelope: %v", err)
	}
	if err := validateToolPairs(envelope.Messages); err != nil {
		t.Fatalf("compacted replay has invalid tool pairs: %v", err)
	}
	if len(envelope.Messages) >= len(messages) {
		t.Fatalf("large replay did not compact: %d messages", len(envelope.Messages))
	}

	target := &ClawExecutor{sessions: newNodeSessionStore()}
	ctx := WithRunID(context.Background(), runID)
	if err := target.UnpackSession(ctx, "claw", sessionID, legacyBlob); err != nil {
		t.Fatalf("unpack legacy oversized replay: %v", err)
	}
	got := target.sessions.load(runID, slot)
	if len(got) == 0 {
		t.Fatal("compacted replay was not restored")
	}
	replayedBlob, err := marshalClawPersistEnvelope(slot, sessionID, "claw:openai", got)
	if err != nil {
		t.Fatalf("marshal restored replay: %v", err)
	}
	if len(replayedBlob) > clawPersistReplayBlobBytes {
		t.Fatalf("restored legacy replay length=%d, want <=%d", len(replayedBlob), clawPersistReplayBlobBytes)
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
