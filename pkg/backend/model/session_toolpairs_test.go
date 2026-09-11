package model

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
	clawrt "github.com/SocialGouv/claw-code-go/pkg/runtime"
	"github.com/SocialGouv/iterion/pkg/backend/delegate"
)

func toolUseMessage(ids ...string) api.Message {
	blocks := make([]api.ContentBlock, 0, len(ids))
	for _, id := range ids {
		blocks = append(blocks, api.ContentBlock{Type: "tool_use", ID: id, Name: "read_file", Input: map[string]any{}})
	}
	return api.Message{Role: "assistant", Content: blocks}
}

func toolResultMessage(ids ...string) api.Message {
	blocks := make([]api.ContentBlock, 0, len(ids))
	for _, id := range ids {
		blocks = append(blocks, api.ToolResult{ToolUseID: id, Content: "ok"}.ToContentBlock())
	}
	return api.Message{Role: "user", Content: blocks}
}

func TestSanitizeToolPairs_ValidTranscriptIsByteIdentical(t *testing.T) {
	in := []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "inspect"}}},
		toolUseMessage("a", "b", "c"),
		toolResultMessage("a"),
		toolResultMessage("b"),
		toolResultMessage("c"),
		{Role: "assistant", Content: []api.ContentBlock{{Type: "text", Text: "done"}}},
	}
	want, _ := json.Marshal(in)
	out, stats := sanitizeToolPairs(in, nil, false)
	got, _ := json.Marshal(out)
	if string(got) != string(want) {
		t.Fatalf("valid transcript changed:\nwant %s\n got %s", want, got)
	}
	if stats.removedBlocks() != 0 || stats.MessagesRemoved != 0 {
		t.Fatalf("unexpected repairs: %+v", stats)
	}
	if err := validateToolPairs(out); err != nil {
		t.Fatalf("valid transcript rejected: %v", err)
	}
}

func TestSanitizeToolPairs_PrunesBothOrphanDirections(t *testing.T) {
	in := []api.Message{
		toolResultMessage("missing-use"),
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "keep me"}}},
		toolUseMessage("paired", "missing-result"),
		toolResultMessage("paired"),
	}
	out, stats := sanitizeToolPairs(in, nil, false)
	if stats.ToolResultsRemoved != 1 || stats.ToolUsesRemoved != 1 || stats.MessagesRemoved != 1 {
		t.Fatalf("repair stats=%+v", stats)
	}
	if err := validateToolPairs(out); err != nil {
		t.Fatalf("repaired transcript invalid: %v", err)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "missing-use") || strings.Contains(string(raw), "missing-result") || !strings.Contains(string(raw), "keep me") {
		t.Fatalf("unexpected repaired transcript: %s", raw)
	}
}

func TestSanitizeToolPairs_PreservesOnlyExplicitPendingAsk(t *testing.T) {
	in := []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "start"}}},
		toolUseMessage("already-ran", "ask-pending"),
	}
	out, stats := sanitizeToolPairs(in, map[string]struct{}{"ask-pending": {}}, false)
	if stats.ToolUsesRemoved != 1 {
		t.Fatalf("repair stats=%+v", stats)
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), "already-ran") || !strings.Contains(string(raw), "ask-pending") {
		t.Fatalf("pending repair=%s", raw)
	}
}

func TestCompactMessagesToolSafe_RepairsBoundaryFromRun4893(t *testing.T) {
	large := strings.Repeat("boundary filler ", 200)
	seed := make([]api.Message, 12)
	for i := range seed {
		seed[i] = api.Message{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: large}}}
	}
	seedRes := clawrt.CompactMessages(seed, clawrt.CompactionConfig{PreserveRecentMessages: 4, MaxEstimatedTokens: 1})
	if seedRes == nil {
		t.Fatal("expected seed compaction")
	}
	// Preserve the existing compacted-summary message found in the real blob.
	// With 13 compactable messages and keep=8, the tool_use is the last block
	// summarized and its tool_result becomes the first retained message.
	in := append([]api.Message(nil), seedRes.CompactedMessages...)
	in = append(in,
		toolUseMessage("call_mLlgpMRfNzIL1t7PmYviSIWg"),
		toolResultMessage("call_mLlgpMRfNzIL1t7PmYviSIWg"),
	)
	for i := 0; i < 7; i++ {
		in = append(in, api.Message{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "recent"}}})
	}
	res := compactMessagesToolSafe(in, clawrt.CompactionConfig{PreserveRecentMessages: 8, MaxEstimatedTokens: 1}, nil)
	if res == nil {
		t.Fatal("expected compaction")
	}
	if len(res.CompactedMessages) >= len(in) {
		t.Fatalf("compaction did not shrink: %d -> %d", len(in), len(res.CompactedMessages))
	}
	if err := validateToolPairs(res.CompactedMessages); err != nil {
		t.Fatalf("boundary repair invalid: %v", err)
	}
	raw, _ := json.Marshal(res.CompactedMessages)
	if strings.Contains(string(raw), "call_mLlgpMRfNzIL1t7PmYviSIWg") {
		t.Fatalf("orphaned boundary result survived: %s", raw)
	}
}

func TestNodeSessionStoreCompactUsesToolSafeWrapper(t *testing.T) {
	const runID, slot = "run-compact", "assistant_conversation"
	large := strings.Repeat("store filler ", 300)
	in := []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: large}}},
		toolUseMessage("store-boundary"),
		toolResultMessage("store-boundary"),
	}
	for i := 0; i < 7; i++ {
		in = append(in, api.Message{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: large}}})
	}
	s := newNodeSessionStore()
	s.save(runID, slot, in)
	if _, fired := s.compact(runID, slot, clawrt.CompactionConfig{PreserveRecentMessages: 8, MaxEstimatedTokens: 1}); !fired {
		t.Fatal("expected store compaction")
	}
	got := s.load(runID, slot)
	if err := validateToolPairs(got); err != nil {
		t.Fatalf("stored compacted transcript invalid: %v", err)
	}
}

func TestForceCompactToTokens_TightBudgetStillShrinks(t *testing.T) {
	large := strings.Repeat("tight budget ", 500)
	in := []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: large}}},
		toolUseMessage("cut-at-boundary"),
		toolResultMessage("cut-at-boundary"),
	}
	for i := 0; i < 8; i++ {
		in = append(in, api.Message{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: large}}})
	}
	out, ok := forceCompactToTokens(in, 1, 8)
	if !ok || len(out) >= len(in) {
		t.Fatalf("tight-budget compaction=%t, messages %d -> %d", ok, len(in), len(out))
	}
	if err := validateToolPairs(out); err != nil {
		t.Fatalf("tight-budget output invalid: %v", err)
	}
}

func TestBuildRequestRejectsMalformedToolTranscript(t *testing.T) {
	_, err := buildRequest(GenerationOptions{Model: "test"}, []api.Message{toolResultMessage("orphan")}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "has no earlier tool_use") {
		t.Fatalf("buildRequest error=%v", err)
	}
}

func TestUnpackSessionRepairsLegacyOrphans(t *testing.T) {
	env := clawPersistEnvelope{
		Version:   clawPersistVersion,
		SessionID: "session-4893",
		Slot:      "assistant_conversation",
		Messages: []api.Message{
			{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "summary"}}},
			toolResultMessage("call_mLlgpMRfNzIL1t7PmYviSIWg"),
			toolUseMessage("paired"),
			toolResultMessage("paired"),
			toolUseMessage("orphaned-tail"),
		},
	}
	blob, _ := json.Marshal(env)
	e := &ClawExecutor{sessions: newNodeSessionStore()}
	if err := e.UnpackSession(WithRunID(context.Background(), "run-4893"), delegate.BackendClaw, env.SessionID, blob); err != nil {
		t.Fatalf("UnpackSession: %v", err)
	}
	got := e.sessions.load("run-4893", env.Slot)
	if err := validateToolPairs(got); err != nil {
		t.Fatalf("repaired persisted transcript invalid: %v", err)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "call_mLlgpMRfNzIL1t7PmYviSIWg") || strings.Contains(string(raw), "orphaned-tail") || !strings.Contains(string(raw), "paired") {
		t.Fatalf("unexpected persisted repair: %s", raw)
	}
}

func TestTaskSessionRollbackAndEvictionUseNamedSlot(t *testing.T) {
	const runID = "run-slot"
	sessions := newNodeSessionStore()
	baseline := []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "remember me"}}}}
	sessions.save(runID, "assistant_conversation", baseline)
	sessions.save(runID, "revise", []api.Message{{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "node residue"}}}})
	ctx := withRuntimeContext(context.Background(), runID, sessions)
	e := &ClawExecutor{}
	task := &delegate.Task{NodeID: "revise", SessionSlot: "assistant_conversation"}

	snapshot := e.snapshotTaskSession(ctx, task)
	sessions.save(runID, "assistant_conversation", []api.Message{
		{Role: "assistant", Content: []api.ContentBlock{{Type: "thinking", Text: "signed partial"}}},
		toolResultMessage("orphan-from-failed-route"),
	})
	e.rollbackTaskSession(ctx, snapshot)
	rolledBack := sessions.load(runID, "assistant_conversation")
	raw, _ := json.Marshal(rolledBack)
	if !strings.Contains(string(raw), "remember me") || strings.Contains(string(raw), "signed partial") {
		t.Fatalf("rollback=%s", raw)
	}

	e.evictTaskSession(ctx, task)
	if got := sessions.load(runID, "assistant_conversation"); got != nil {
		t.Fatalf("named slot survived explicit eviction: %#v", got)
	}
	if got := sessions.load(runID, "revise"); len(got) == 0 {
		t.Fatal("node-id residue was evicted instead of the named slot")
	}
}
