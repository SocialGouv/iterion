package model

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/SocialGouv/claw-code-go/pkg/api"
)

// TestTurnCaptureIsUniformAcrossGenerationModes locks the invariant the
// timeline and the Fork API both stand on: EVERY LLM turn leaves a
// TurnCheckpoint anchor, whichever generation mode produced it.
//
// It was not always so. Capture lived only in GenerateTextDirect, so a node
// declaring an `output:` schema — which in iterion is most of them — ran
// through GenerateObjectDirect and anchored NOTHING: its run showed an empty
// timeline and `iterion fork` could not target it at all ("turn not found").
// A live test caught it; this one keeps it caught without an API call.
//
// The assertion is deliberately mode-BY-mode rather than a single happy
// path: the failure was one mode silently lacking what the other had, so a
// test that exercised only one would have stayed green through it.
func TestTurnCaptureIsUniformAcrossGenerationModes(t *testing.T) {
	type out struct {
		OK bool `json:"ok"`
	}
	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"]}`)
	userMsg := []api.Message{
		{Role: "user", Content: []api.ContentBlock{{Type: "text", Text: "Are you running?"}}},
	}

	t.Run("structured generation captures its turn", func(t *testing.T) {
		client := newMockClient([]api.StreamEvent{
			{Type: api.EventMessageStart, InputTokens: 12},
			{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "tool_use", Index: 0, ID: "tu_1", Name: "structured_output"}},
			{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "input_json_delta", PartialJSON: `{"ok":true}`}},
			{Type: api.EventContentBlockStop, Index: 0},
			{Type: api.EventMessageDelta, StopReason: "tool_use", Usage: api.UsageDelta{OutputTokens: 3}},
			{Type: api.EventMessageStop},
		})

		var captured []TurnCaptureInfo
		if _, err := GenerateObjectDirect[out](context.Background(), client, GenerationOptions{
			Model:          "claude-sonnet-4-6",
			ExplicitSchema: schema,
			Messages:       userMsg,
			OnTurnCapture:  func(i TurnCaptureInfo) { captured = append(captured, i) },
		}); err != nil {
			t.Fatalf("GenerateObjectDirect: %v", err)
		}

		if len(captured) != 1 {
			t.Fatalf("turn captures = %d, want exactly 1 — a structured call is one turn, and it must anchor the timeline and the Fork API like any other", len(captured))
		}
		// The snapshot has to carry the conversation, not just a marker:
		// Fork rehydrates a child from these messages, so an empty one
		// would anchor a turn nobody can actually resume from.
		if len(captured[0].Conversation) == 0 {
			t.Error("captured turn carries no conversation — Fork has nothing to rehydrate from")
		}
	})

	t.Run("text generation captures its turn", func(t *testing.T) {
		client := newMockClient([]api.StreamEvent{
			{Type: api.EventMessageStart, InputTokens: 12},
			{Type: api.EventContentBlockStart, Index: 0, ContentBlock: api.ContentBlockInfo{Type: "text", Index: 0}},
			{Type: api.EventContentBlockDelta, Index: 0, Delta: api.Delta{Type: "text_delta", Text: "yes"}},
			{Type: api.EventContentBlockStop, Index: 0},
			{Type: api.EventMessageDelta, StopReason: "end_turn", Usage: api.UsageDelta{OutputTokens: 2}},
			{Type: api.EventMessageStop},
		})

		var captured []TurnCaptureInfo
		if _, err := GenerateTextDirect(context.Background(), client, GenerationOptions{
			Model:         "claude-sonnet-4-6",
			Messages:      userMsg,
			OnTurnCapture: func(i TurnCaptureInfo) { captured = append(captured, i) },
		}); err != nil {
			t.Fatalf("GenerateTextDirect: %v", err)
		}

		if len(captured) != 1 {
			t.Fatalf("turn captures = %d, want exactly 1", len(captured))
		}
		if len(captured[0].Conversation) == 0 {
			t.Error("captured turn carries no conversation")
		}
	})
}
